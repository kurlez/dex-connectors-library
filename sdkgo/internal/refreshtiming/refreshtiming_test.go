// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package refreshtiming

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var issuedAt = time.Date(2026, time.October, 1, 9, 0, 0, 0, time.UTC)

func expiresAfter(lifetime time.Duration) *time.Time {
	expiresAt := issuedAt.Add(lifetime)
	return &expiresAt
}

func TestSkewAndForcedRefreshFloorFollowTheLifetime(t *testing.T) {
	for _, test := range []struct {
		name          string
		lifetime      time.Duration
		expectedSkew  time.Duration
		expectedFloor time.Duration
	}{
		{name: "one hour", lifetime: time.Hour, expectedSkew: 5 * time.Minute, expectedFloor: 60 * time.Second},
		{name: "twenty minutes reaches the skew cap exactly", lifetime: 20 * time.Minute, expectedSkew: 5 * time.Minute, expectedFloor: 60 * time.Second},
		{name: "ten minutes", lifetime: 10 * time.Minute, expectedSkew: 150 * time.Second, expectedFloor: 60 * time.Second},
		{name: "four minutes reaches the floor cap exactly", lifetime: 4 * time.Minute, expectedSkew: time.Minute, expectedFloor: 60 * time.Second},
		{name: "sixty seconds", lifetime: time.Minute, expectedSkew: 15 * time.Second, expectedFloor: 15 * time.Second},
		{name: "four seconds", lifetime: 4 * time.Second, expectedSkew: time.Second, expectedFloor: time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			expiresAt := expiresAfter(test.lifetime)
			require.Equal(t, test.lifetime, Lifetime(&issuedAt, expiresAt))
			require.Equal(t, test.expectedSkew, Skew(&issuedAt, expiresAt))
			require.Equal(t, test.expectedFloor, ForcedRefreshFloor(&issuedAt, expiresAt))
		})
	}
}

func TestUnknownOrNonPositiveLifetimeUsesTheMaximums(t *testing.T) {
	beforeIssue := issuedAt.Add(-time.Minute)
	for name, times := range map[string][2]*time.Time{
		"no issue time":       {nil, expiresAfter(time.Minute)},
		"no expiry":           {&issuedAt, nil},
		"neither":             {nil, nil},
		"expiry at issue":     {&issuedAt, &issuedAt},
		"expiry before issue": {&issuedAt, &beforeIssue},
	} {
		t.Run(name, func(t *testing.T) {
			require.Zero(t, Lifetime(times[0], times[1]))
			require.Equal(t, MaximumSkew, Skew(times[0], times[1]))
			require.Equal(t, MaximumForcedRefreshFloor, ForcedRefreshFloor(times[0], times[1]))
		})
	}
}

func TestExpiryWithinSkewIsInclusiveAtTheBoundary(t *testing.T) {
	expiresAt := expiresAfter(time.Minute)
	boundary := expiresAt.Add(-15 * time.Second)
	require.False(t, IsExpiryWithinSkew(&issuedAt, expiresAt, boundary.Add(-time.Nanosecond)))
	require.True(t, IsExpiryWithinSkew(&issuedAt, expiresAt, boundary))
	require.True(t, IsExpiryWithinSkew(&issuedAt, expiresAt, *expiresAt))
	require.True(t, IsExpiryWithinSkew(nil, expiresAt, expiresAt.Add(-5*time.Minute)), "an unknown lifetime uses the full skew")
}
