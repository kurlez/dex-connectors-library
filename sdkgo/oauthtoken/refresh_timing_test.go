// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package oauthtoken

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIsRefreshRequired(t *testing.T) {
	now := time.Date(2026, time.September, 30, 8, 0, 0, 0, time.UTC)
	insideSkew := now.Add(RefreshSkew)
	beyondSkew := insideSkew.Add(time.Nanosecond)
	for _, test := range []struct {
		name              string
		hasAccessToken    bool
		expiresAt         *time.Time
		rule              MissingExpiryRule
		isRefreshRequired bool
	}{
		{name: "absent access token", expiresAt: &beyondSkew, rule: KeepWhenExpiryMissing, isRefreshRequired: true},
		{name: "missing expiry refreshes", hasAccessToken: true, rule: RefreshWhenExpiryMissing, isRefreshRequired: true},
		{name: "missing expiry keeps non-expiring token", hasAccessToken: true, rule: KeepWhenExpiryMissing},
		{name: "expiry inside skew", hasAccessToken: true, expiresAt: &insideSkew, rule: KeepWhenExpiryMissing, isRefreshRequired: true},
		{name: "expiry beyond skew", hasAccessToken: true, expiresAt: &beyondSkew, rule: RefreshWhenExpiryMissing},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.isRefreshRequired, IsRefreshRequired(test.hasAccessToken, test.expiresAt, now, test.rule))
		})
	}
}

func TestRefreshSkewKeepsItsValue(t *testing.T) {
	require.Equal(t, 5*time.Minute, RefreshSkew)
}

func TestProportionalSkewAndForcedRefreshFloorForTokenLifetimes(t *testing.T) {
	issuedAt := time.Date(2026, time.September, 30, 8, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name          string
		issuedAt      *time.Time
		lifetime      time.Duration
		expectedSkew  time.Duration
		expectedFloor time.Duration
	}{
		{name: "sixty seconds", issuedAt: &issuedAt, lifetime: time.Minute, expectedSkew: 15 * time.Second, expectedFloor: 15 * time.Second},
		{name: "ten minutes", issuedAt: &issuedAt, lifetime: 10 * time.Minute, expectedSkew: 150 * time.Second, expectedFloor: time.Minute},
		{name: "one hour", issuedAt: &issuedAt, lifetime: time.Hour, expectedSkew: 5 * time.Minute, expectedFloor: time.Minute},
		{name: "unknown issue time", lifetime: time.Minute, expectedSkew: RefreshSkew, expectedFloor: time.Minute},
		{name: "expiry at issue", issuedAt: &issuedAt, expectedSkew: RefreshSkew, expectedFloor: time.Minute},
		{name: "expiry before issue", issuedAt: &issuedAt, lifetime: -time.Hour, expectedSkew: RefreshSkew, expectedFloor: time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			expiresAt := issuedAt.Add(test.lifetime)
			require.Equal(t, test.expectedSkew, ProportionalRefreshSkew(test.issuedAt, &expiresAt))
			require.Equal(t, test.expectedFloor, ForcedRefreshFloor(test.issuedAt, &expiresAt))
		})
	}
	require.Equal(t, RefreshSkew, ProportionalRefreshSkew(&issuedAt, nil))
	require.Equal(t, time.Minute, ForcedRefreshFloor(&issuedAt, nil))
}

func TestIsRefreshRequiredWithIssueTimeUsesTheProportionalSkew(t *testing.T) {
	issuedAt := time.Date(2026, time.September, 30, 8, 0, 0, 0, time.UTC)
	expiresAt := issuedAt.Add(time.Minute)
	insideSkew := expiresAt.Add(-15 * time.Second)
	beforeSkew := insideSkew.Add(-time.Nanosecond)
	for _, test := range []struct {
		name              string
		hasAccessToken    bool
		issuedAt          *time.Time
		expiresAt         *time.Time
		now               time.Time
		rule              MissingExpiryRule
		isRefreshRequired bool
	}{
		{name: "absent access token", expiresAt: &expiresAt, now: issuedAt, rule: KeepWhenExpiryMissing, isRefreshRequired: true},
		{name: "missing expiry refreshes", hasAccessToken: true, issuedAt: &issuedAt, now: issuedAt, rule: RefreshWhenExpiryMissing, isRefreshRequired: true},
		{name: "missing expiry keeps non-expiring token", hasAccessToken: true, issuedAt: &issuedAt, now: issuedAt, rule: KeepWhenExpiryMissing},
		{name: "sixty second token just after issue", hasAccessToken: true, issuedAt: &issuedAt, expiresAt: &expiresAt, now: issuedAt, rule: KeepWhenExpiryMissing},
		{name: "before the proportional skew", hasAccessToken: true, issuedAt: &issuedAt, expiresAt: &expiresAt, now: beforeSkew, rule: KeepWhenExpiryMissing},
		{name: "at the proportional skew", hasAccessToken: true, issuedAt: &issuedAt, expiresAt: &expiresAt, now: insideSkew, rule: KeepWhenExpiryMissing, isRefreshRequired: true},
		{name: "unknown issue time uses the full skew", hasAccessToken: true, expiresAt: &expiresAt, now: issuedAt, rule: KeepWhenExpiryMissing, isRefreshRequired: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.isRefreshRequired, IsRefreshRequiredWithIssueTime(test.hasAccessToken, test.issuedAt, test.expiresAt, test.now, test.rule))
		})
	}
}
