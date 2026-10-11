// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package providerhttp_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/superdurable/dex-connectors-library/sdkgo/providerhttp"
)

// stubNetwork stands in for DNS and the network: every host name resolves to the addresses a test sets, and
// every connection really goes to target, so the test sees which address the dialer chose to connect to.
type stubNetwork struct {
	target    string
	addresses []netip.Addr
	mu        sync.Mutex
	dialed    []string
}

func newStubNetwork(t *testing.T, target string, addresses ...string) *stubNetwork {
	t.Helper()
	network := &stubNetwork{target: target}
	for _, address := range addresses {
		network.addresses = append(network.addresses, netip.MustParseAddr(address))
	}
	return network
}

func (network *stubNetwork) resolve(context.Context, string) ([]netip.Addr, error) {
	return append([]netip.Addr(nil), network.addresses...), nil
}

func (network *stubNetwork) dial(ctx context.Context, networkName string, address string) (net.Conn, error) {
	network.mu.Lock()
	network.dialed = append(network.dialed, address)
	network.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, networkName, network.target)
}

func (network *stubNetwork) dialedAddresses() []string {
	network.mu.Lock()
	defer network.mu.Unlock()
	return append([]string(nil), network.dialed...)
}

type addressServers struct {
	plain, secure *httptest.Server
}

func newAddressServers(t *testing.T) addressServers {
	t.Helper()
	handler := http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) { _, _ = response.Write([]byte("ok")) })
	servers := addressServers{plain: httptest.NewServer(handler), secure: httptest.NewTLSServer(handler)}
	t.Cleanup(servers.plain.Close)
	t.Cleanup(servers.secure.Close)
	return servers
}

// get requests a host name that resolves to addresses, with the policy under test. The test certificate of
// httptest is valid for example.com.
func (servers addressServers) get(t *testing.T, policy providerhttp.AddressPolicy, scheme string, addresses ...string) (*stubNetwork, error) {
	t.Helper()
	server := servers.plain
	if scheme == "https" {
		server = servers.secure
	}
	network := newStubNetwork(t, server.Listener.Addr().String(), addresses...)
	client, err := providerhttp.NewAddressPolicyClientForTest(server.Client(), 5*time.Second, policy, network.resolve, network.dial)
	require.NoError(t, err)
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	response, err := client.Get(scheme + "://example.com:" + parsed.Port() + "/")
	if err == nil {
		require.NoError(t, response.Body.Close())
		require.Equal(t, http.StatusOK, response.StatusCode)
	}
	return network, err
}

// TestAddressPoliciesAllowAndRefuseEveryResolvedAddressSet runs one table under both policies. Each row says
// whether PrivateNetworkHTTP and PublicHTTPS connect over plain http and over https to a host name that
// resolves to the row's addresses.
func TestAddressPoliciesAllowAndRefuseEveryResolvedAddressSet(t *testing.T) {
	servers := newAddressServers(t)
	for _, test := range []struct {
		name                                   string
		addresses                              []string
		privateHTTP, privateHTTPS, publicHTTPS bool
	}{
		{"IPv4 loopback", []string{"127.0.0.1"}, true, true, false},
		{"IPv6 loopback", []string{"::1"}, true, true, false},
		{"10.0.0.0/8", []string{"10.1.2.3"}, true, true, false},
		{"172.16.0.0/12", []string{"172.16.0.5"}, true, true, false},
		{"192.168.0.0/16", []string{"192.168.1.1"}, true, true, false},
		{"IPv6 unique local", []string{"fd12:3456::1"}, true, true, false},
		{"several private addresses", []string{"10.0.0.1", "192.168.0.2", "::1"}, true, true, false},
		{"IPv4-mapped private address is dialed as IPv4", []string{"::ffff:10.0.0.1"}, true, true, false},
		{"IPv4-mapped public address is dialed as IPv4", []string{"::ffff:8.8.8.8"}, false, true, true},
		{"public IPv4", []string{"8.8.8.8"}, false, true, true},
		{"public IPv6", []string{"2001:4860:4860::8888"}, false, true, true},
		{"several public addresses", []string{"8.8.8.8", "1.1.1.1"}, false, true, true},
		{"carrier-grade NAT is not private and not public", []string{"100.64.0.1"}, false, true, false},
		{"one public address among private ones", []string{"10.0.0.1", "8.8.8.8"}, false, true, false},
		{"one private address among public ones", []string{"8.8.8.8", "10.0.0.1"}, false, true, false},
		{"cloud metadata address", []string{"169.254.169.254"}, false, false, false},
		{"another link-local IPv4 address", []string{"169.254.0.1"}, false, false, false},
		{"IPv6 link-local", []string{"fe80::1"}, false, false, false},
		{"IPv4-mapped metadata address", []string{"::ffff:169.254.169.254"}, false, false, false},
		{"IPv6 metadata address", []string{"fd00:ec2::254"}, false, false, false},
		{"loopback together with the metadata address", []string{"127.0.0.1", "169.254.169.254"}, false, false, false},
		{"public address together with the metadata address", []string{"8.8.8.8", "169.254.169.254"}, false, false, false},
		{"unspecified IPv4 address", []string{"0.0.0.0"}, false, false, false},
		{"unspecified IPv6 address", []string{"::"}, false, false, false},
		{"multicast address", []string{"224.0.0.1"}, false, false, false},
	} {
		for _, scenario := range []struct {
			policy      providerhttp.AddressPolicy
			policyName  string
			scheme      string
			isPermitted bool
		}{
			{providerhttp.PrivateNetworkHTTP, "PrivateNetworkHTTP", "http", test.privateHTTP},
			{providerhttp.PrivateNetworkHTTP, "PrivateNetworkHTTP", "https", test.privateHTTPS},
			{providerhttp.PublicHTTPS, "PublicHTTPS", "http", false},
			{providerhttp.PublicHTTPS, "PublicHTTPS", "https", test.publicHTTPS},
		} {
			t.Run(scenario.policyName+"/"+scenario.scheme+"/"+test.name, func(t *testing.T) {
				network, err := servers.get(t, scenario.policy, scenario.scheme, test.addresses...)

				if scenario.isPermitted {
					require.NoError(t, err)
					dialed := network.dialedAddresses()
					require.Len(t, dialed, 1)
					host, _, splitErr := net.SplitHostPort(dialed[0])
					require.NoError(t, splitErr)
					require.Equal(t, netip.MustParseAddr(test.addresses[0]).Unmap().String(), host, "the client connects to the address it checked, not to the name")
					return
				}
				require.ErrorIs(t, err, providerhttp.ErrAddressNotAllowed)
				var refusal *providerhttp.AddressRefusal
				require.ErrorAs(t, err, &refusal)
				require.NotEmpty(t, refusal.Reason)
				require.NotContains(t, refusal.Reason, "example.com", "a refusal names the rule, not the host")
				require.Empty(t, network.dialedAddresses(), "a refused address is never dialed")
			})
		}
	}
}

func TestAddressPoliciesGiveEachRefusalItsRule(t *testing.T) {
	servers := newAddressServers(t)
	for _, test := range []struct {
		policy         providerhttp.AddressPolicy
		scheme         string
		address        string
		expectedReason string
	}{
		{providerhttp.PrivateNetworkHTTP, "https", "169.254.169.254", "link-local"},
		{providerhttp.PublicHTTPS, "https", "fe80::1", "link-local"},
		{providerhttp.PrivateNetworkHTTP, "https", "0.0.0.0", "unicast"},
		{providerhttp.PrivateNetworkHTTP, "http", "8.8.8.8", "loopback or private"},
		{providerhttp.PublicHTTPS, "http", "8.8.8.8", "plain http is not allowed"},
		{providerhttp.PublicHTTPS, "https", "10.0.0.1", "only public addresses"},
		{providerhttp.PublicHTTPS, "https", "100.64.0.1", "only public addresses"},
	} {
		_, err := servers.get(t, test.policy, test.scheme, test.address)

		var refusal *providerhttp.AddressRefusal
		require.ErrorAs(t, err, &refusal, "%s %s", test.scheme, test.address)
		require.Contains(t, refusal.Reason, test.expectedReason, "%s %s", test.scheme, test.address)
	}
}

func TestAddressPoliciesRefuseLiteralAddressesWithTheSystemNetwork(t *testing.T) {
	for _, policy := range []providerhttp.AddressPolicy{providerhttp.PrivateNetworkHTTP, providerhttp.PublicHTTPS} {
		client, err := providerhttp.NewAddressPolicyClient(nil, 2*time.Second, policy)
		require.NoError(t, err)
		for _, address := range []string{
			"https://169.254.169.254/latest/api/token", "http://169.254.169.254/latest/api/token", "http://[fe80::1]/",
			"https://[::ffff:169.254.169.254]/", "http://0.0.0.0:8080/", "http://8.8.8.8/",
		} {
			_, err = client.Get(address)
			require.ErrorIs(t, err, providerhttp.ErrAddressNotAllowed, "policy %d, %s", policy, address)
		}
	}
	loopback := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) { _, _ = response.Write([]byte("ok")) }))
	t.Cleanup(loopback.Close)
	privateClient, err := providerhttp.NewAddressPolicyClient(nil, 2*time.Second, providerhttp.PrivateNetworkHTTP)
	require.NoError(t, err)
	response, err := privateClient.Get(loopback.URL)
	require.NoError(t, err, "plain http to a loopback server works with the system resolver and dialer")
	require.NoError(t, response.Body.Close())
	publicClient, err := providerhttp.NewAddressPolicyClient(nil, 2*time.Second, providerhttp.PublicHTTPS)
	require.NoError(t, err)
	_, err = publicClient.Get(loopback.URL)
	require.ErrorIs(t, err, providerhttp.ErrAddressNotAllowed, "PublicHTTPS refuses a loopback server")
}

func TestNewAddressPolicyClientKeepsTLSSettingsAndIgnoresProxies(t *testing.T) {
	servers := newAddressServers(t)
	proxyWasUsed := false
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxyWasUsed = true }))
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	require.NoError(t, err)
	caller := servers.secure.Client()
	transport := caller.Transport.(*http.Transport)
	transport.Proxy = http.ProxyURL(proxyURL)
	network := newStubNetwork(t, servers.secure.Listener.Addr().String(), "8.8.8.8")
	client, err := providerhttp.NewAddressPolicyClientForTest(caller, time.Second, providerhttp.PublicHTTPS, network.resolve, network.dial)
	require.NoError(t, err)
	parsed, err := url.Parse(servers.secure.URL)
	require.NoError(t, err)

	response, err := client.Get("https://example.com:" + parsed.Port() + "/")

	require.NoError(t, err, "the caller's root certificates still verify the server")
	require.NoError(t, response.Body.Close())
	require.False(t, proxyWasUsed, "a proxy would receive the connection instead of the address that was checked")
	require.NotNil(t, transport.Proxy, "the caller's transport is not changed")
	require.Equal(t, time.Second, client.Timeout, "the request timeout applies when the caller sets none")
	require.Zero(t, caller.Timeout)
}

func TestNewAddressPolicyClientNeverFollowsRedirects(t *testing.T) {
	var redirectedRequests int
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirectedRequests++ }))
	t.Cleanup(target.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)
	client, err := providerhttp.NewAddressPolicyClient(nil, time.Second, providerhttp.PrivateNetworkHTTP)
	require.NoError(t, err)

	response, err := client.Get(origin.URL)

	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusTemporaryRedirect, response.StatusCode)
	require.Zero(t, redirectedRequests)
}

func TestNewAddressPolicyClientRejectsWhatItCannotGuard(t *testing.T) {
	_, err := providerhttp.NewAddressPolicyClient(&http.Client{Transport: roundTripperFunc(nil)}, time.Second, providerhttp.PrivateNetworkHTTP)
	require.ErrorContains(t, err, "*http.Transport")
	for _, policy := range []providerhttp.AddressPolicy{0, -1, 3} {
		_, err = providerhttp.NewAddressPolicyClient(nil, time.Second, policy)
		require.ErrorContains(t, err, "not defined")
	}
}

func TestNewAddressPolicyClientKeepsThePolicyOfAClientItReturned(t *testing.T) {
	servers := newAddressServers(t)
	network := newStubNetwork(t, servers.plain.Listener.Addr().String(), "10.0.0.1")
	first, err := providerhttp.NewAddressPolicyClientForTest(nil, time.Second, providerhttp.PrivateNetworkHTTP, network.resolve, network.dial)
	require.NoError(t, err)

	second, err := providerhttp.NewAddressPolicyClient(first, 3*time.Second, providerhttp.PublicHTTPS)
	require.NoError(t, err)

	parsed, err := url.Parse(servers.plain.URL)
	require.NoError(t, err)
	response, err := second.Get("http://internal.test:" + parsed.Port() + "/")
	require.NoError(t, err, "the second call keeps the first call's policy and network")
	require.NoError(t, response.Body.Close())
	require.Equal(t, []string{net.JoinHostPort("10.0.0.1", parsed.Port())}, network.dialedAddresses())
}

func TestAddressRefusalMatchesTheSentinelAndNamesOnlyTheRule(t *testing.T) {
	refusal := &providerhttp.AddressRefusal{Reason: "the rule"}

	require.True(t, errors.Is(refusal, providerhttp.ErrAddressNotAllowed))
	require.Equal(t, "address not allowed: the rule", refusal.Error())
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
