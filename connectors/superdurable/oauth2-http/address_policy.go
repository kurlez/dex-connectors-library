// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package oauth2http

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/superdurable/dex-connectors-library/sdkgo/providerhttp"
)

const dialTimeout = 10 * time.Second

// errAddressNotAllowed marks a connection that the address policy refused before any byte was sent.
var errAddressNotAllowed = errors.New("address not allowed")

// awsIPv6MetadataAddress is the EC2 instance metadata service on IPv6. It is a private address, so only a
// named rule keeps it out of reach of plain http.
var awsIPv6MetadataAddress = netip.MustParseAddr("fd00:ec2::254")

// addressRefusal is the typed error of a refused connection. Its reason names the rule, never the host.
type addressRefusal struct{ reason string }

// Error names the rule that refused the address.
func (refusal *addressRefusal) Error() string {
	return errAddressNotAllowed.Error() + ": " + refusal.reason
}

// Unwrap lets errors.Is match errAddressNotAllowed.
func (*addressRefusal) Unwrap() error { return errAddressNotAllowed }

// addressPolicy decides which addresses the connector connects to. Every connection resolves the host
// once, checks every resolved address, and dials only addresses it checked, so a name that resolves
// differently on a second lookup cannot redirect the token.
//
//   - Plain http connects only when every resolved address is loopback or private.
//   - https connects to any address that passes the rule below.
//   - A link-local address (169.254.0.0/16 and fe80::/10, which hold the cloud metadata service), an
//     unspecified or multicast address, and the IPv6 metadata address are never allowed, even over https.
type addressPolicy struct {
	lookupAddresses func(ctx context.Context, host string) ([]netip.Addr, error)
	dial            func(ctx context.Context, network string, address string) (net.Conn, error)
}

func defaultAddressPolicy() addressPolicy {
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	return addressPolicy{
		lookupAddresses: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		dial: dialer.DialContext,
	}
}

// dialContext returns the dial function of a transport that serves plain http or https requests.
func (policy addressPolicy) dialContext(isPlainHTTP bool) func(ctx context.Context, network string, address string) (net.Conn, error) {
	return func(ctx context.Context, network string, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := policy.lookupAddresses(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(addresses) == 0 {
			return nil, &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
		}
		for index := range addresses {
			addresses[index] = addresses[index].WithZone("").Unmap()
			if reason := refusalReason(addresses[index], isPlainHTTP); reason != "" {
				return nil, &addressRefusal{reason: reason}
			}
		}
		var dialErrors error
		for _, checkedAddress := range addresses {
			connection, dialErr := policy.dial(ctx, network, net.JoinHostPort(checkedAddress.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			dialErrors = errors.Join(dialErrors, dialErr)
		}
		return nil, dialErrors
	}
}

// refusalReason returns why address is refused, or an empty string when it is allowed.
func refusalReason(address netip.Addr, isPlainHTTP bool) string {
	switch {
	case address.IsLinkLocalUnicast(), address.IsLinkLocalMulticast(), address == awsIPv6MetadataAddress:
		return "link-local addresses, which include the cloud metadata address, are never allowed"
	case address.IsUnspecified(), address.IsMulticast():
		return "only unicast addresses are allowed"
	case isPlainHTTP && !address.IsLoopback() && !address.IsPrivate():
		return "plain http is allowed only when every resolved address is loopback or private; use https"
	}
	return ""
}

// addressPolicyTransport sends http requests through a transport whose dialer allows loopback and private
// addresses, and https requests through one that allows every address except those that are never allowed.
type addressPolicyTransport struct {
	plainHTTP *http.Transport
	secure    *http.Transport
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

// newAddressPolicyClient returns a copy of caller that is safe for credentialed calls: it never follows a
// redirect, applies requestTimeout when caller sets none, and connects only as policy allows. caller's
// transport must be nil or an *http.Transport, whose TLS and connection-pool settings the copy keeps; the
// copy ignores its proxy settings, because a proxy would receive the connection instead of the checked
// address. A client that an earlier call returned keeps its policy.
func newAddressPolicyClient(caller *http.Client, requestTimeout time.Duration, policy addressPolicy) (*http.Client, error) {
	var copied http.Client
	var callerTransport http.RoundTripper
	if caller != nil {
		copied, callerTransport = *caller, caller.Transport
	}
	switch transport := callerTransport.(type) {
	case *addressPolicyTransport:
	case nil:
		copied.Transport = newAddressPolicyTransport(defaultHTTPTransport(), policy)
	case *http.Transport:
		copied.Transport = newAddressPolicyTransport(transport, policy)
	default:
		return nil, errors.New("OAuth 2.0 HTTP client transport must be nil or an *http.Transport so the connector can enforce its address policy")
	}
	return providerhttp.NewProviderHTTPClient(&copied, requestTimeout), nil
}

func newAddressPolicyTransport(base *http.Transport, policy addressPolicy) *addressPolicyTransport {
	return &addressPolicyTransport{plainHTTP: policyClone(base, policy.dialContext(true)), secure: policyClone(base, policy.dialContext(false))}
}

func policyClone(base *http.Transport, dialContext func(context.Context, string, string) (net.Conn, error)) *http.Transport {
	clone := base.Clone()
	clone.Proxy = nil
	clone.DialContext = dialContext
	// Any other dial hook of the caller's transport would bypass the policy.
	clone.Dial, clone.DialTLS, clone.DialTLSContext = nil, nil, nil
	return clone
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

// describeRefusal returns the connector-level text for a refused connection of target, such as "apiBaseUrl".
func describeRefusal(target string, refusal *addressRefusal) string {
	return fmt.Sprintf("the connection's %s resolves to an address the connector will not connect to: %s", target, refusal.reason)
}
