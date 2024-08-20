package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	testZone string = "myip.opendns.com."
	timeout  int    = 5
)

// MockDNSClient is a mock implementation of the DNSClient interface
type MockDNSClient struct {
	mock.Mock
}

func (m *MockDNSClient) ExchangeContext(ctx context.Context, msg *dns.Msg, address string) (*dns.Msg, time.Duration, error) {
	args := m.Called(msg, address)
	return args.Get(0).(*dns.Msg), args.Get(1).(time.Duration), args.Error(2)
}

// MockHTTPClient is a mock implementation of the HTTPClient interface
type MockHTTPClient struct {
	mock.Mock
}

func (m *MockHTTPClient) Do(req *http.Request) (*http.Response, error) {
	args := m.Called(req)
	return args.Get(0).(*http.Response), args.Error(1)
}

func TestFetchIPViaDNS(t *testing.T) {
	// Prepare the mock DNS client
	mockClient := new(MockDNSClient)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(timeout)*time.Second,
	)
	defer cancel()

	dns.Id = func() uint16 { return 1234 }

	// Prepare the DNS message and response
	msg := new(dns.Msg)
	msg.SetQuestion("myip.opendns.com.", dns.TypeA)
	msg.RecursionDesired = true
	msg.Id = 1234

	response := new(dns.Msg)
	response.Answer = append(response.Answer, &dns.A{
		A: net.IPv4(192, 0, 2, 1),
	})
	response.Id = 1234

	mockClient.On(
		"ExchangeContext",
		msg,
		"resolver1.opendns.com:53",
	).Return(response, time.Duration(0), nil)

	// Call the function under test
	ip, err := FetchIPViaDNS(ctx, mockClient, testZone)

	// Verify the results
	require.NoError(t, err)
	assert.Equal(t, "192.0.2.1", ip)

	mockClient.AssertExpectations(t)
}

func TestFetchIPViaDNS_EmptyARecord(t *testing.T) {
	// Prepare the mock DNS client
	mockClient := new(MockDNSClient)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(timeout)*time.Second,
	)
	defer cancel()

	dns.Id = func() uint16 { return 1234 }

	// Prepare the DNS message and response
	msg := new(dns.Msg)
	msg.SetQuestion("myip.opendns.com.", dns.TypeA)
	msg.RecursionDesired = true
	msg.Id = 1234

	response := new(dns.Msg)
	response.Answer = []dns.RR{}
	response.Id = 1234

	mockClient.On(
		"ExchangeContext",
		msg,
		"resolver1.opendns.com:53",
	).Return(response, time.Duration(0), nil)

	// Call the function under test
	ip, err := FetchIPViaDNS(ctx, mockClient, testZone)

	// Verify the results
	require.Error(t, err)
	assert.EqualError(t, err, "no A record found in DNS query")
	assert.Empty(t, ip)

	mockClient.AssertExpectations(t)
}

func TestFetchIPViaDNS_Error(t *testing.T) {
	// Prepare the mock DNS client
	mockClient := new(MockDNSClient)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(timeout)*time.Second,
	)
	defer cancel()

	// Make the ids static otherwise the test fails
	dns.Id = func() uint16 { return 1234 }

	// Prepare the DNS message and an empty response
	msg := new(dns.Msg)
	msg.SetQuestion("myip.opendns.com.", dns.TypeA)
	response := new(dns.Msg) // No Answer

	mockClient.On(
		"ExchangeContext",
		msg,
		"resolver1.opendns.com:53",
	).Return(response, time.Duration(0), nil)

	// Call the function under test
	ip, err := FetchIPViaDNS(ctx, mockClient, testZone)

	// Verify the results
	require.Error(t, err)
	assert.Empty(t, ip)
}

func TestFetchIPViaDNS_Timeout(t *testing.T) {
	// Prepare the mock DNS client
	mockClient := new(MockDNSClient)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(timeout)*time.Second,
	)
	defer cancel()

	// Simulate a DNS timeout by returning an error
	msg := new(dns.Msg)
	msg.SetQuestion("myip.opendns.com.", dns.TypeA)
	msg.RecursionDesired = true

	mockClient.On("ExchangeContext", msg, "resolver1.opendns.com:53").Return(
		(*dns.Msg)(nil),
		time.Duration(0),
		errors.New("timeout"),
	)

	// Call the function under test
	ip, err := FetchIPViaDNS(ctx, mockClient, testZone)

	// Verify that a timeout error is returned
	require.Error(t, err)
	assert.Empty(t, ip)
	assert.Contains(t, err.Error(), "timeout")

	// Assert the method was called with expected arguments
	mockClient.AssertExpectations(t)
}

func TestFetchIPViaDNS_NetworkError(t *testing.T) {
	mockClient := new(MockDNSClient)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(timeout)*time.Second,
	)
	defer cancel()

	dns.Id = func() uint16 { return 1234 }

	msg := new(dns.Msg)
	msg.SetQuestion("myip.opendns.com.", dns.TypeA)
	msg.RecursionDesired = true

	mockClient.On("ExchangeContext", msg, "resolver1.opendns.com:53").Return((*dns.Msg)(nil), time.Duration(0), errors.New("network error"))

	ip, err := FetchIPViaDNS(ctx, mockClient, testZone)
	require.Error(t, err)
	assert.Empty(t, ip)
	assert.Contains(t, err.Error(), "network error")
}

func TestFetchIPViaDNS_IncorrectAddress(t *testing.T) {
	mockClient := new(MockDNSClient)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(timeout)*time.Second,
	)
	defer cancel()

	dns.Id = func() uint16 { return 1234 }

	msg := new(dns.Msg)
	msg.SetQuestion("myip.opendns.com.", dns.TypeA)
	msg.RecursionDesired = true

	mockClient.On("ExchangeContext", msg, "resolver1.opendns.com:53").Return(
		(*dns.Msg)(nil),
		time.Duration(0),
		errors.New("unreachable"),
	)

	ip, err := FetchIPViaDNS(ctx, mockClient, testZone)
	require.Error(t, err)
	assert.Empty(t, ip)
	assert.Contains(t, err.Error(), "unreachable")
}

func TestFetchIPViaHTTP(t *testing.T) {
	// Prepare the mock HTTP client
	mockClient := new(MockHTTPClient)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(timeout)*time.Second,
	)
	defer cancel()

	// Prepare the HTTP response
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("192.0.2.1")),
	}

	mockClient.On("Do", mock.Anything).Return(response, nil)

	// Call the function under test
	ip, err := FetchIPViaHTTP(ctx, mockClient)

	// Verify the results
	require.NoError(t, err)
	assert.Equal(t, "192.0.2.1", ip)
}

func TestFetchIPViaHTTP_Error(t *testing.T) {
	// Prepare the mock HTTP client
	mockClient := new(MockHTTPClient)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(timeout)*time.Second,
	)
	defer cancel()

	mockClient.On("Do", mock.Anything).Return(
		(*http.Response)(nil),
		errors.New("failed to connect"),
	)

	// Call the function under test
	ip, err := FetchIPViaHTTP(ctx, mockClient)

	// Verify the results
	require.Error(t, err)
	assert.Empty(t, ip)
}

func TestFetchIPViaHTTP_Non200Status(t *testing.T) {
	mockClient := new(MockHTTPClient)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(timeout)*time.Second,
	)
	defer cancel()

	mockClient.On("Do", mock.Anything).Return(&http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(http.StatusText(http.StatusInternalServerError))),
	}, nil)

	ip, err := FetchIPViaHTTP(ctx, mockClient)
	require.Error(t, err)
	assert.Empty(t, ip)
	assert.Contains(t, err.Error(), "unexpected status code")
}

func TestFetchIPViaHTTP_BodyReadError(t *testing.T) {
	mockClient := new(MockHTTPClient)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(timeout)*time.Second,
	)
	defer cancel()

	body := io.NopCloser(&errReader{})
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       body,
	}

	mockClient.On("Do", mock.Anything).Return(response, nil)

	ip, err := FetchIPViaHTTP(ctx, mockClient)
	require.Error(t, err)
	assert.Empty(t, ip)
	assert.Contains(t, err.Error(), "failed to read response body")
}

// errReader simulates an error when reading from an io.Reader
type errReader struct{}

func (*errReader) Read(p []byte) (n int, err error) {
	return 0, errors.New("read error")
}
