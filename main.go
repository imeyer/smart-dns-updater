package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// DNSClient defines an interface for a DNS client
type DNSClient interface {
	Exchange(msg *dns.Msg, address string) (*dns.Msg, time.Duration, error)
}

// HTTPClient defines an interface for making HTTP GET requests
type HTTPClient interface {
	Get(url string) (*http.Response, error)
}

// FetchIPViaDNS fetches the IP using the dig command from the go-dns library
func FetchIPViaDNS(client DNSClient) (string, error) {
	msg := new(dns.Msg)

	msg.SetQuestion("myip.opendns.com.", dns.TypeA)
	msg.RecursionDesired = true

	r, _, err := client.Exchange(msg, "resolver1.opendns.com:53")
	if err != nil {
		return "", fmt.Errorf("failed to exchange with DNS server: %v", err)
	}

	if len(r.Answer) == 0 {
		return "", fmt.Errorf("no answer from DNS query")
	}

	for _, ans := range r.Answer {
		if aRecord, ok := ans.(*dns.A); ok {
			return aRecord.A.String(), nil
		}
	}

	return "", fmt.Errorf("no A record found in DNS query")
}

// FetchIPViaHTTP fetches the IP using HTTP GET request to ifconfig.me
func FetchIPViaHTTP(client HTTPClient) (string, error) {
	resp, err := client.Get("http://ifconfig.me/ip")
	if err != nil {
		return "", fmt.Errorf("failed to get IP via HTTP: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %v", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %v", err)
	}

	return strings.TrimSpace(string(body)), nil
}

func main() {
	timeout := flag.Int("timeout", 5, "timeout in seconds")
	flag.Parse()

	_, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(*timeout)*time.Second,
	)
	defer cancel()

	var dnsIp string
	var httpIp string
	var err error

	dnsIp, err = FetchIPViaDNS(new(dns.Client))
	if err != nil {
		fmt.Printf("Error fetching IP: %v\n", err)
		os.Exit(1)
	}

	httpIp, err = FetchIPViaHTTP(&http.Client{})

	if err != nil {
		fmt.Printf("Error fetching IP: %v\n", err)
		os.Exit(1)
	}

	if dnsIp != httpIp {
		fmt.Printf("IPs do not match")
	}

	fmt.Printf("Your IP from DNS is: %s\n", dnsIp)
	fmt.Printf("Your IP from HTTP is: %s\n", httpIp)
}
