package main

import (
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

// MockDNSClient is a mock implementation of the DNSClient interface
type MockDNSClient struct {
	mock.Mock
}

func (m *MockDNSClient) Exchange(msg *dns.Msg, address string) (*dns.Msg, time.Duration, error) {
	args := m.Called(msg, address)
	return args.Get(0).(*dns.Msg), args.Get(1).(time.Duration), args.Error(2)
}

// MockHTTPClient is a mock implementation of the HTTPClient interface
type MockHTTPClient struct {
	mock.Mock
}

func (m *MockHTTPClient) Get(url string) (*http.Response, error) {
	args := m.Called(url)
	return args.Get(0).(*http.Response), args.Error(1)
}

func TestFetchIPViaDNS(t *testing.T) {
	// Prepare the mock DNS client
	mockClient := new(MockDNSClient)

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

	mockClient.On("Exchange", msg, "resolver1.opendns.com:53").Return(response, time.Duration(0), nil)

	// Call the function under test
	ip, err := FetchIPViaDNS(mockClient)

	// Verify the results
	require.NoError(t, err)
	assert.Equal(t, "192.0.2.1", ip)

	mockClient.AssertExpectations(t)
}

func TestFetchIPViaDNS_Error(t *testing.T) {
	// Prepare the mock DNS client
	mockClient := new(MockDNSClient)

	// Make the ids static otherwise the test fails
	dns.Id = func() uint16 { return 1234 }

	// Prepare the DNS message and an empty response
	msg := new(dns.Msg)
	msg.SetQuestion("myip.opendns.com.", dns.TypeA)
	response := new(dns.Msg) // No Answer

	mockClient.On(
		"Exchange",
		msg,
		"resolver1.opendns.com:53",
	).Return(response, time.Duration(0), nil)

	// Call the function under test
	ip, err := FetchIPViaDNS(mockClient)

	// Verify the results
	require.Error(t, err)
	assert.Empty(t, ip)
}

func TestFetchIPViaDNS_Timeout(t *testing.T) {
	// Prepare the mock DNS client
	mockClient := new(MockDNSClient)

	// Simulate a DNS timeout by returning an error
	msg := new(dns.Msg)
	msg.SetQuestion("myip.opendns.com.", dns.TypeA)
	msg.RecursionDesired = true

	mockClient.On("Exchange", msg, "resolver1.opendns.com:53").Return(
		(*dns.Msg)(nil),
		time.Duration(0),
		errors.New("timeout"),
	)

	// Call the function under test
	ip, err := FetchIPViaDNS(mockClient)

	// Verify that a timeout error is returned
	require.Error(t, err)
	assert.Empty(t, ip)
	assert.Contains(t, err.Error(), "timeout")

	// Assert the method was called with expected arguments
	mockClient.AssertExpectations(t)
}

func TestFetchIPViaDNS_NetworkError(t *testing.T) {
	mockClient := new(MockDNSClient)

	dns.Id = func() uint16 { return 1234 }

	msg := new(dns.Msg)
	msg.SetQuestion("myip.opendns.com.", dns.TypeA)
	msg.RecursionDesired = true

	mockClient.On("Exchange", msg, "resolver1.opendns.com:53").Return((*dns.Msg)(nil), time.Duration(0), errors.New("network error"))

	ip, err := FetchIPViaDNS(mockClient)
	require.Error(t, err)
	assert.Empty(t, ip)
	assert.Contains(t, err.Error(), "network error")
}

func TestFetchIPViaDNS_IncorrectAddress(t *testing.T) {
	mockClient := new(MockDNSClient)

	dns.Id = func() uint16 { return 1234 }

	msg := new(dns.Msg)
	msg.SetQuestion("myip.opendns.com.", dns.TypeA)
	msg.RecursionDesired = true

	mockClient.On("Exchange", msg, "resolver1.opendns.com:53").Return((*dns.Msg)(nil), time.Duration(0), errors.New("unreachable"))

	ip, err := FetchIPViaDNS(mockClient)
	require.Error(t, err)
	assert.Empty(t, ip)
	assert.Contains(t, err.Error(), "unreachable")
}

func TestFetchIPViaHTTP(t *testing.T) {
	// Prepare the mock HTTP client
	mockClient := new(MockHTTPClient)

	// Prepare the HTTP response
	body := "192.0.2.1\n"
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	mockClient.On("Get", "http://ifconfig.me/ip").Return(response, nil)

	// Call the function under test
	ip, err := FetchIPViaHTTP(mockClient)

	// Verify the results
	require.NoError(t, err)
	assert.Equal(t, "192.0.2.1", ip)
}

func TestFetchIPViaHTTP_Error(t *testing.T) {
	// Prepare the mock HTTP client
	mockClient := new(MockHTTPClient)

	// Mock a failure
	mockClient.On("Get", "http://ifconfig.me/ip").Return(
		(*http.Response)(nil),
		errors.New("failed to connect"),
	)

	// Call the function under test
	ip, err := FetchIPViaHTTP(mockClient)

	// Verify the results
	require.Error(t, err)
	assert.Empty(t, ip)
}

func TestFetchIPViaHTTP_Non200Status(t *testing.T) {
	mockClient := new(MockHTTPClient)

	response := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader("")),
	}

	mockClient.On("Get", "http://ifconfig.me/ip").Return(response, nil)

	ip, err := FetchIPViaHTTP(mockClient)
	require.Error(t, err)
	assert.Empty(t, ip)
	assert.Contains(t, err.Error(), "unexpected status code")
}

func TestFetchIPViaHTTP_BodyReadError(t *testing.T) {
	mockClient := new(MockHTTPClient)

	body := io.NopCloser(&errReader{})
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       body,
	}

	mockClient.On("Get", "http://ifconfig.me/ip").Return(response, nil)

	ip, err := FetchIPViaHTTP(mockClient)
	require.Error(t, err)
	assert.Empty(t, ip)
	assert.Contains(t, err.Error(), "failed to read response body")
}

// errReader simulates an error when reading from an io.Reader
type errReader struct{}

func (*errReader) Read(p []byte) (n int, err error) {
	return 0, errors.New("read error")
}
