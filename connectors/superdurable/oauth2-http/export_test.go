// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package oauth2http

import (
	"context"
	"net"
	"net/netip"
)

// WithAddressResolutionForTest replaces DNS resolution and the TCP dial of the address policy, so a test
// chooses the addresses a host resolves to and where the connection really goes.
func WithAddressResolutionForTest(
	resolve func(ctx context.Context, host string) ([]netip.Addr, error),
	dial func(ctx context.Context, network string, address string) (net.Conn, error),
) Option {
	return func(options *clientOptions) {
		options.addressPolicy = addressPolicy{lookupAddresses: resolve, dial: dial}
	}
}
