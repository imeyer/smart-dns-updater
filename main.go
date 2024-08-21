package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	gdns "google.golang.org/api/dns/v1"
)

var (
	fetchDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "smart_dns_updater",
			Name:      "fetch_method_duration_seconds",
			Help:      "Duration of fetch operations by method",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"fetch_method"},
	)

	isIPOutOfSyncDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "smart_dns_updater",
			Name:      "is_out_of_sync_duration_seconds",
			Help:      "Duration of the isIPOutOfSync function",
			Buckets:   prometheus.DefBuckets,
		},
	)

	fetchErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "smart_dns_updater",
			Name:      "fetch_method_errors_total",
			Help:      "Total number of fetch errors by method",
		},
		[]string{"fetch_method"},
	)

	updateErrors = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: "smart_dns_updater",
			Name:      "dns_updates_errors_total",
			Help:      "Total number of dns update errors",
		},
	)

	updatesApplied = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: "smart_dns_updater",
			Name:      "dns_updates_applied_total",
			Help:      "Total number of dns updates applied",
		},
	)

	outOfSync = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "smart_dns_updater",
			Name:      "out_of_sync",
			Help:      "Is DNS out of sync",
		},
	)

	versionGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "smart_dns_updater",
		Name:      "build_info",
		Help:      "smart-dns-updater build information",
	}, []string{"version", "git_commit"})

	// Build information
	version  string     = "dev"
	gitSha   string     = "no-commit"
	logLevel slog.Level = slog.LevelInfo
)

func init() {
	// Register the Prometheus metrics
	prometheus.MustRegister(fetchDuration)
	prometheus.MustRegister(isIPOutOfSyncDuration)
	prometheus.MustRegister(fetchErrors)
	prometheus.MustRegister(outOfSync)
	prometheus.MustRegister(updateErrors)
	prometheus.MustRegister(updatesApplied)
	prometheus.MustRegister(versionGauge)

	versionGauge.With(prometheus.Labels{"version": version, "git_commit": gitSha}).Set(1)

	// Initialize the error counters to 0
	fetchErrors.WithLabelValues("dns").Add(0)
	fetchErrors.WithLabelValues("http").Add(0)
	updateErrors.Add(0)

	// Initialize update counter to 0
	updatesApplied.Add(0)
}

type Config struct {
	ProjectID         string `toml:"project_id"`
	Zone              string `toml:"zone"`
	TickTime          int    `toml:"tick_time"`
	Timeout           int    `toml:"timeout"`
	PreferredResponse string `toml:"preferred_response"`
	RecordName        string `toml:"record_name"`
}

type DNSClient interface {
	ExchangeContext(
		ctx context.Context,
		msg *dns.Msg,
		address string,
	) (*dns.Msg, time.Duration, error)
}

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

func fetchIPViaDNS(ctx context.Context, client DNSClient, question string) (string, error) {
	timer := prometheus.NewTimer(fetchDuration.WithLabelValues("dns"))
	defer timer.ObserveDuration()

	msg := new(dns.Msg)
	msg.SetQuestion(question, dns.TypeA)
	msg.RecursionDesired = true

	r, _, err := client.ExchangeContext(ctx, msg, "resolver1.opendns.com:53")
	if err != nil {
		fetchErrors.WithLabelValues("dns").Inc()
		return "", fmt.Errorf("failed to exchange with DNS server: %w", err)
	}

	if len(r.Answer) == 0 {
		fetchErrors.WithLabelValues("dns").Inc()
		return "", fmt.Errorf("no A record found in DNS query")
	}

	return r.Answer[0].(*dns.A).A.String(), nil
}

func fetchIPViaHTTP(ctx context.Context, client HTTPClient) (string, error) {
	timer := prometheus.NewTimer(fetchDuration.WithLabelValues("http"))
	defer timer.ObserveDuration()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://ifconfig.me/ip", nil)

	resp, err := client.Do(req)
	if err != nil {
		fetchErrors.WithLabelValues("http").Inc()
		return "", fmt.Errorf("failed to get IP via HTTP: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fetchErrors.WithLabelValues("http").Inc()
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fetchErrors.WithLabelValues("http").Inc()
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	return strings.TrimSpace(string(body)), nil
}

func loadConfig(configPath string) (*Config, error) {
	config := &Config{}
	configFile, err := os.Open(configPath)
	if err != nil {
		return nil, fmt.Errorf("could not open config file: %w", err)
	}
	defer configFile.Close()

	decoder := toml.NewDecoder(configFile)
	if _, err := decoder.Decode(config); err != nil {
		return nil, fmt.Errorf("could not decode config file: %w", err)
	}
	return config, nil
}

func newLogger(logLevel *slog.Level) *slog.Logger {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
		Level:     logLevel,
	}))
	slog.SetDefault(logger)

	return logger
}

func reconcileDns(httpClient HTTPClient, dnsClient DNSClient, config *Config, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(config.Timeout)*time.Second,
	)

	// Synchronize the fetch calls
	var wg sync.WaitGroup
	var dnsIp, prodIp, httpIp string
	var dnsErr, prodErr, httpErr error

	wg.Add(3)

	// Fetch IP via DNS for my production domain
	go func() {
		defer wg.Done()
		prodIp, prodErr = fetchIPViaDNS(ctx, dnsClient, "imeyer.io.")
		if dnsErr != nil {
			logger.Error("error fetching IP from DNS", "error", prodErr)
		}
	}()

	// Fetch IP via DNS
	go func() {
		defer wg.Done()
		dnsIp, dnsErr = fetchIPViaDNS(ctx, dnsClient, "myip.opendns.com.")
		if dnsErr != nil {
			logger.Error("error fetching IP from DNS", "error", dnsErr)
		}
	}()

	// Fetch IP via HTTP
	go func() {
		defer wg.Done()
		httpIp, httpErr = fetchIPViaHTTP(ctx, httpClient)
		if httpErr != nil {
			logger.Error("error fetching IP from HTTP", "error", httpErr)
		}
	}()

	// Wait for all goroutines to finish
	wg.Wait()

	// After all goroutines have returned, compare the results if no errors
	if dnsErr == nil && httpErr == nil && prodErr == nil {
		if ip, outofsync := isIPOutOfSync(dnsIp, httpIp, prodIp, config.PreferredResponse, logger); outofsync {
			logger.Info("ips out of sync", "prod_ip", prodIp, "fetched_ip", ip)
			if err := updateDNS(config, ip, logger); err != nil {
				logger.Error(err.Error())
			}
		} else {
			logger.Info("all IPs match", "dns_ip", dnsIp, "http_ip", httpIp, "prod_ip", prodIp)
		}
	} else {
		logger.Error("errors in responses", "dns_err", dnsErr, "http_err", httpErr, "prod_err", prodErr)
	}

	// Cancel the context
	cancel()
}

func updateDNS(config *Config, ip string, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(config.Timeout)*time.Second,
	)
	defer cancel()

	if ip == "" {
		return fmt.Errorf("ip is empty")
	}

	client, err := gdns.NewService(ctx)
	if err != nil {
		return fmt.Errorf("failed to create DNS client: %v", err)
	}

	rrs := []*gdns.ResourceRecordSet{
		{
			Name:    config.RecordName,
			Rrdatas: []string{ip},
			Type:    "A",
			Ttl:     3600,
		},
	}

	resp, err := client.ResourceRecordSets.List(config.ProjectID, config.Zone).Name(config.RecordName).Type("A").Do()
	if err != nil {
		return fmt.Errorf("failed to list DNS records: %v", err)
	}

	changes := &gdns.Change{
		Additions: rrs,
		Deletions: resp.Rrsets,
	}

	_, err = client.Changes.Create(config.ProjectID, config.Zone, changes).Context(ctx).Do()
	if err != nil {
		updateErrors.Inc()
		return fmt.Errorf("failed to update DNS: %v", err)
	}

	logger.Info("dns changes applied", "record", config.RecordName, "ip", ip)
	updatesApplied.Inc()
	return nil
}

func isIPOutOfSync(dns, http, prod, preferred string, logger *slog.Logger) (string, bool) {
	timer := prometheus.NewTimer(isIPOutOfSyncDuration)
	defer timer.ObserveDuration()

	if dns != http {
		logger.Warn("DNS and HTTP IPs do not match", "dns", dns, "http", http)
	}

	var finalIp string

	switch preferred {
	case "dns":
		finalIp = dns
	case "http":
		finalIp = http
	default:
		finalIp = ""
	}

	if prod != finalIp {
		outOfSync.Set(1)
		return finalIp, true
	}

	outOfSync.Set(0)
	return "", false
}

func main() {
	var configPath string
	var debug bool

	flag.StringVar(&configPath, "config", "smart-dns-updater.toml", "path to the config file")
	flag.BoolVar(&debug, "debug", false, "enable debug logging")
	flag.Parse()

	if debug {
		logLevel = slog.LevelDebug
	}

	logger := newLogger(&logLevel)

	logger.Info("starting smart-dns-updater", "version", version, "git_sha", gitSha)

	config, err := loadConfig(configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	logger.Debug("", "config", fmt.Sprintf("%#v", config))

	server := &http.Server{
		Addr:         ":39387",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Expose Prometheus metrics
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		if err := server.ListenAndServe(); err != nil {
			logger.Error("error spawning http server for /metrics")
		}
	}()

	ticker := time.NewTicker(time.Duration(config.TickTime) * time.Second)
	defer ticker.Stop()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	dnsClient := new(dns.Client)
	httpClient := &http.Client{}

	logger.Info(
		"starting main thread",
		"preferred_response",
		config.PreferredResponse,
		"fetch_timeout",
		config.Timeout,
	)

	// reconcile once then wait for the Ticker duration
	reconcileDns(httpClient, dnsClient, config, logger)

	for {
		select {
		case sig := <-sigChan:
			// Exit with the UNIX standard of 128+signal number
			if sigNum, ok := sig.(syscall.Signal); ok {
				s := 128 + int(sigNum)
				logger.Info("Received signal, exiting gracefully", "signal", sig.String())
				os.Exit(s)
			} else {
				// Exit with 1 implies being killed by NOT a signal
				os.Exit(1)
			}
		case <-ticker.C:
			reconcileDns(httpClient, dnsClient, config, logger)
		}
	}
}
