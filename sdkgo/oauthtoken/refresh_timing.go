// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package oauthtoken

import (
	"time"

	"github.com/superdurable/dex-connectors-library/sdkgo/internal/refreshtiming"
)

// RefreshSkew is how long before expiry an access token counts as expired, so
// an operation never starts with a token that can expire during the call. It is also the
// maximum of ProportionalRefreshSkew, which shortens it for short lifetimes.
const RefreshSkew = refreshtiming.MaximumSkew

// MissingExpiryRule decides whether an access token without a recorded expiry
// needs a refresh.
type MissingExpiryRule int

const (
	// RefreshWhenExpiryMissing refreshes the token. Use it when the provider
	// issues only expiring access tokens, such as Google.
	RefreshWhenExpiryMissing MissingExpiryRule = iota + 1
	// KeepWhenExpiryMissing keeps the token as the provider's supported
	// non-expiring form, such as a GitHub Enterprise or non-rotating Slack token.
	KeepWhenExpiryMissing
)

// ProportionalRefreshSkew returns how long before expiresAt an access token counts as expired:
// min(RefreshSkew, lifetime/4), where lifetime is expiresAt minus issuedAt. A fixed five minutes
// would count a 60-second token as expired on issue. It returns RefreshSkew when either time is
// nil or the lifetime is not positive.
func ProportionalRefreshSkew(issuedAt, expiresAt *time.Time) time.Duration {
	return refreshtiming.Skew(issuedAt, expiresAt)
}

// ForcedRefreshFloor returns how long after issuedAt a credential that a provider rejected is
// not refreshed again: min(60 seconds, lifetime/4), where lifetime is expiresAt minus issuedAt.
// It returns 60 seconds when either time is nil or the lifetime is not positive. A holder of the
// credential that is rejected within the floor waits for a retry instead of forcing another refresh.
func ForcedRefreshFloor(issuedAt, expiresAt *time.Time) time.Duration {
	return refreshtiming.ForcedRefreshFloor(issuedAt, expiresAt)
}

// IsRefreshRequired reports whether an access token must be refreshed before
// use at now: it is absent, it has no expiry and rule says to refresh, or it
// expires within RefreshSkew. It is deterministic for one input, as
// sdkgo.CredentialRefreshDriver.RefreshRequired requires. It does not know when the token was
// issued, so it always applies the full RefreshSkew; use IsRefreshRequiredWithIssueTime when the
// issue time is known.
func IsRefreshRequired(hasAccessToken bool, expiresAt *time.Time, now time.Time, rule MissingExpiryRule) bool {
	return IsRefreshRequiredWithIssueTime(hasAccessToken, nil, expiresAt, now, rule)
}

// IsRefreshRequiredWithIssueTime reports whether an access token must be refreshed before use at
// now, like IsRefreshRequired, but it counts the token as expired within ProportionalRefreshSkew
// of expiresAt. A nil issuedAt gives IsRefreshRequired's result.
func IsRefreshRequiredWithIssueTime(hasAccessToken bool, issuedAt, expiresAt *time.Time, now time.Time, rule MissingExpiryRule) bool {
	if !hasAccessToken {
		return true
	}
	if expiresAt == nil {
		return rule != KeepWhenExpiryMissing
	}
	return refreshtiming.IsExpiryWithinSkew(issuedAt, expiresAt, now)
}
