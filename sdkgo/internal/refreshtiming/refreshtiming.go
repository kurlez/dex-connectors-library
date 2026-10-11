// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

// Package refreshtiming decides when an expiring credential needs a refresh. It imports only the standard
// library so projectconfig, which Dex Server and Dex Web import, shares the rule with oauthtoken without
// loading the Dex Worker SDK.
package refreshtiming

import "time"

// MaximumSkew is the longest time before expiry at which a credential counts as expired.
const MaximumSkew = 5 * time.Minute

// MaximumForcedRefreshFloor is the longest time after issue during which a rejected credential is not refreshed again.
const MaximumForcedRefreshFloor = 60 * time.Second

// lifetimeFraction divides a lifetime into the share that skew and the forced-refresh floor may take.
const lifetimeFraction = 4

// Lifetime returns expiresAt minus issuedAt, or zero when either time is unknown or the lifetime is not positive.
func Lifetime(issuedAt, expiresAt *time.Time) time.Duration {
	if issuedAt == nil || expiresAt == nil || !expiresAt.After(*issuedAt) {
		return 0
	}
	return expiresAt.Sub(*issuedAt)
}

// Skew returns min(MaximumSkew, lifetime/4), or MaximumSkew when the lifetime is unknown.
func Skew(issuedAt, expiresAt *time.Time) time.Duration {
	return shareOfLifetime(MaximumSkew, Lifetime(issuedAt, expiresAt))
}

// ForcedRefreshFloor returns min(MaximumForcedRefreshFloor, lifetime/4), or MaximumForcedRefreshFloor when the lifetime is unknown.
func ForcedRefreshFloor(issuedAt, expiresAt *time.Time) time.Duration {
	return shareOfLifetime(MaximumForcedRefreshFloor, Lifetime(issuedAt, expiresAt))
}

// IsExpiryWithinSkew reports whether expiresAt is no later than now plus Skew.
func IsExpiryWithinSkew(issuedAt, expiresAt *time.Time, now time.Time) bool {
	return !expiresAt.After(now.Add(Skew(issuedAt, expiresAt)))
}

func shareOfLifetime(maximum, lifetime time.Duration) time.Duration {
	if lifetime == 0 {
		return maximum
	}
	return min(maximum, lifetime/lifetimeFraction)
}
