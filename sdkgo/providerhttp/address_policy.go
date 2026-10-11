// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package providerhttp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"
)

const addressPolicyDialTimeout = 10 * time.Second

// AddressPolicy selects which connections NewAddressPolicyClient allows. A host name is resolved once, every
// resolved address is checked, and the client connects only to an address it checked. A link-local address
// (169.254.0.0/16 and fe80::/10, which hold the cloud metadata service), the IPv6 metadata address
// fd00:ec2::254, and unspecified and multicast addresses are refused under every policy, even over https.
type AddressPolicy int

const (
	// PublicHTTPS allows only https, and only when every resolved address is public: not loopback, private,
	// or in the shared address space 100.64.0.0/10. A hosting platform uses it for provider calls whose
	// URLs it does not control.
	PublicHTTPS AddressPolicy = iota + 1
	// PrivateNetworkHTTP allows https to every address that is not always refused, and plain http only when
	// every resolved address is loopback or private (127.0.0.0/8, ::1, 10.0.0.0/8, 172.16.0.0/12,
	// 192.168.0.0/16, fc00::/7). A connector whose URLs the connection owner enters uses it, because only
	// the owner knows whether the server runs on a private network.
	PrivateNetworkHTTP
)

// ErrAddressNotAllowed matches, through errors.Is, a connection that an AddressPolicy refused before any
// byte was sent.
var ErrAddressNotAllowed = errors.New("address not allowed")

// AddressRefusal is the error of a connection that an AddressPolicy refused. It names the rule that applied,
// never the host or the address. Find it with errors.As.
type AddressRefusal struct {
	// Reason is the rule that refused the connection, as a sentence fragment such as "plain http is not allowed".
	Reason string
}

// Error returns ErrAddressNotAllowed's text followed by the reason.
func (refusal *AddressRefusal) Error() string {
	return ErrAddressNotAllowed.Error() + ": " + refusal.Reason
}

// Unwrap lets errors.Is match ErrAddressNotAllowed.
func (*AddressRefusal) Unwrap() error { return ErrAddressNotAllowed }

var (
	awsIPv6MetadataAddress = netip.MustParseAddr("fd00:ec2::254")
	sharedAddressSpace     = netip.MustParsePrefix("100.64.0.0/10")
)

// addressNetwork resolves host names and opens connections for an address policy.
type addressNetwork struct {
	lookupAddresses func(ctx context.Context, host string) ([]netip.Addr, error)
	dial            func(ctx context.Context, network string, address string) (net.Conn, error)
}

func systemAddressNetwork() addressNetwork {
	dialer := &net.Dialer{Timeout: addressPolicyDialTimeout, KeepAlive: 30 * time.Second}
	return addressNetwork{
		lookupAddresses: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		dial: dialer.DialContext,
	}
}

// NewAddressPolicyClient returns a copy of caller that is safe for credentialed provider calls. Like
// NewProviderHTTPClient it never follows a redirect and applies requestTimeout when caller sets none.
// It also connects only as policy allows.
//
// caller's transport must be nil or an *http.Transport; any other transport returns an error, because the
// client must own the dialer. The copy keeps the transport's TLS and connection-pool settings and ignores
// its proxy settings, because a proxy would receive the connection instead of the address that was checked.
// The caller's client and transport are not changed. A client that an earlier call returned keeps the
// policy it was built with.
//
// A refused connection fails with an error that matches ErrAddressNotAllowed and holds an *AddressRefusal.
func NewAddressPolicyClient(caller *http.Client, requestTimeout time.Duration, policy AddressPolicy) (*http.Client, error) {
	return newAddressPolicyClient(caller, requestTimeout, policy, systemAddressNetwork())
}

// NewAddressPolicyClientForTest is NewAddressPolicyClient with the name resolution and the TCP dial replaced,
// so a test chooses the addresses a host resolves to and where the connection really goes. dial receives the
// checked address as "ip:port".
func NewAddressPolicyClientForTest(
	caller *http.Client,
	requestTimeout time.Duration,
	policy AddressPolicy,
	resolve func(ctx context.Context, host string) ([]netip.Addr, error),
	dial func(ctx context.Context, network string, address string) (net.Conn, error),
) (*http.Client, error) {
	return newAddressPolicyClient(caller, requestTimeout, policy, addressNetwork{lookupAddresses: resolve, dial: dial})
}

func newAddressPolicyClient(caller *http.Client, requestTimeout time.Duration, policy AddressPolicy, network addressNetwork) (*http.Client, error) {
	if policy != PublicHTTPS && policy != PrivateNetworkHTTP {
		return nil, fmt.Errorf("provider HTTP address policy %d is not defined", policy)
	}
	var copied http.Client
	var callerTransport http.RoundTripper
	if caller != nil {
		copied, callerTransport = *caller, caller.Transport
	}
	switch transport := callerTransport.(type) {
	case *addressPolicyTransport:
	case nil:
		copied.Transport = newAddressPolicyTransport(defaultHTTPTransport(), policy, network)
	case *http.Transport:
		copied.Transport = newAddressPolicyTransport(transport, policy, network)
	default:
		return nil, errors.New("provider HTTP client transport must be nil or an *http.Transport so the client can enforce its address policy")
	}
	return NewProviderHTTPClient(&copied, requestTimeout), nil
}

// addressPolicyTransport sends http requests through a transport whose dialer applies the policy to plain
// http, and https requests through one that applies it to TLS connections.
type addressPolicyTransport struct {
	plainHTTP *http.Transport
	secure    *http.Transport
}

func newAddressPolicyTransport(base *http.Transport, policy AddressPolicy, network addressNetwork) *addressPolicyTransport {
	return &addressPolicyTransport{
		plainHTTP: cloneWithDialer(base, policy.dialContext(network, true)),
		secure:    cloneWithDialer(base, policy.dialContext(network, false)),
	}
}

func cloneWithDialer(base *http.Transport, dialContext func(context.Context, string, string) (net.Conn, error)) *http.Transport {
	clone := base.Clone()
	clone.Proxy = nil
	clone.DialContext = dialContext
	// Any other dial hook of the caller's transport would bypass the policy.
	clone.Dial, clone.DialTLS, clone.DialTLSContext = nil, nil, nil
	return clone
}

// RoundTrip sends request through the transport that matches its scheme.
func (transport *addressPolicyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme == "http" {
		return transport.plainHTTP.RoundTrip(request)
	}
	return transport.secure.RoundTrip(request)
}

// CloseIdleConnections lets http.Client.CloseIdleConnections reach both transports.
func (transport *addressPolicyTransport) CloseIdleConnections() {
	transport.plainHTTP.CloseIdleConnections()
	transport.secure.CloseIdleConnections()
}

// defaultHTTPTransport mirrors http.DefaultTransport's pooling and timeouts.
func defaultHTTPTransport() *http.Transport {
	if base, isTransport := http.DefaultTransport.(*http.Transport); isTransport {
		return base
	}
	return &http.Transport{
		ForceAttemptHTTP2: true, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: time.Second,
	}
}

// dialContext returns the dial function of a transport that serves plain http or https requests. It resolves
// the host once, checks every resolved address, and dials only addresses it checked.
func (policy AddressPolicy) dialContext(network addressNetwork, isPlainHTTP bool) func(ctx context.Context, networkName string, address string) (net.Conn, error) {
	return func(ctx context.Context, networkName string, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := network.lookupAddresses(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(addresses) == 0 {
			return nil, &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
		}
		checked := make([]netip.Addr, len(addresses))
		for index, resolved := range addresses {
			checked[index] = resolved.WithZone("").Unmap()
			if reason := policy.refusalReason(checked[index], isPlainHTTP); reason != "" {
				return nil, &AddressRefusal{Reason: reason}
			}
		}
		var dialErrors error
		for _, checkedAddress := range checked {
			connection, dialErr := network.dial(ctx, networkName, net.JoinHostPort(checkedAddress.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			dialErrors = errors.Join(dialErrors, dialErr)
		}
		return nil, dialErrors
	}
}

// refusalReason returns why policy refuses address, or an empty string when it allows it.
func (policy AddressPolicy) refusalReason(address netip.Addr, isPlainHTTP bool) string {
	isPrivateNetwork := address.IsLoopback() || address.IsPrivate()
	switch {
	case address.IsLinkLocalUnicast(), address.IsLinkLocalMulticast(), address == awsIPv6MetadataAddress:
		return "link-local addresses, which include the cloud metadata address, are never allowed"
	case address.IsUnspecified(), address.IsMulticast():
		return "only unicast addresses are allowed"
	case policy == PublicHTTPS && isPlainHTTP:
		return "plain http is not allowed; use https"
	case policy == PublicHTTPS && (isPrivateNetwork || sharedAddressSpace.Contains(address)):
		return "only public addresses are allowed"
	case policy == PrivateNetworkHTTP && isPlainHTTP && !isPrivateNetwork:
		return "plain http is allowed only when every resolved address is loopback or private; use https"
	}
	return ""
}
