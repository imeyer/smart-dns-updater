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
			Name:      "is_out_of_sync_duration",
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

	outOfSync = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "smart_dns_updater",
			Name:      "out_of_sync",
			Help:      "Total number of fetch errors by method",
		},
		[]string{"preferred_response"},
	)

	versionGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "smart_dns_updater",
		Name:      "build_info",
		Help:      "A gauge with version and git commit information",
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
	prometheus.MustRegister(versionGauge)

	versionGauge.With(prometheus.Labels{"version": version, "git_commit": gitSha}).Set(1)

	// Initialize the error counters to 0
	fetchErrors.WithLabelValues("dns").Add(0)
	fetchErrors.WithLabelValues("http").Add(0)
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

func FetchIPViaDNS(ctx context.Context, client DNSClient, question string) (string, error) {
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

func FetchIPViaHTTP(ctx context.Context, client HTTPClient) (string, error) {
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

	config, err := LoadConfig(configPath)
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

func newLogger(logLevel *slog.Level) *slog.Logger {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
		Level:     logLevel,
	}))
	slog.SetDefault(logger)

	return logger
}

func LoadConfig(configPath string) (*Config, error) {
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
		prodIp, prodErr = FetchIPViaDNS(ctx, dnsClient, "imeyer.io.")
		if dnsErr != nil {
			logger.Error("error fetching IP from DNS", "error", prodErr)
		}
	}()

	// Fetch IP via DNS
	go func() {
		defer wg.Done()
		dnsIp, dnsErr = FetchIPViaDNS(ctx, dnsClient, "myip.opendns.com.")
		if dnsErr != nil {
			logger.Error("error fetching IP from DNS", "error", dnsErr)
		}
	}()

	// Fetch IP via HTTP
	go func() {
		defer wg.Done()
		httpIp, httpErr = FetchIPViaHTTP(ctx, httpClient)
		if httpErr != nil {
			logger.Error("error fetching IP from HTTP", "error", httpErr)
		}
	}()

	// Wait for all goroutines to finish
	wg.Wait()

	// Cancel the context
	cancel()

	// After all goroutines have returned, compare the results if no errors
	if dnsErr == nil && httpErr == nil && prodErr == nil {
		logger.Info("fetched prod ip", "prod_ip", prodIp)
		if ip, outofsync := isIPOutOfSync(dnsIp, httpIp, prodIp, config.PreferredResponse, config, logger); outofsync {
			logger.Info("ips out of sync", "prod_ip", prodIp, "fetched_ip", ip)
		}
	} else {
		logger.Error("errors in responses", "dns_err", dnsErr, "http_err", httpErr, "prod_err", prodErr)
	}

	// TODO: func UpdateDNS() {}
}

func isIPOutOfSync(dns, http, prod, preferred string, config *Config, logger *slog.Logger) (string, bool) {
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
		outOfSync.With(prometheus.Labels{"preferred_response": config.PreferredResponse}).Set(1)
		return finalIp, true
	}

	outOfSync.With(prometheus.Labels{"preferred_response": config.PreferredResponse}).Set(0)
	return "", false
}
