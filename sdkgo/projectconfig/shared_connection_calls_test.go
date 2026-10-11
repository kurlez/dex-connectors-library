// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package projectconfig_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/superdurable/dex-connectors-library/sdkgo/projectconfig"
)

func callContext(callID string) context.Context {
	return projectconfig.ContextWithCallID(context.Background(), callID)
}

func rejectedRevision(request brokerRequest) any { return request.body["rejectedRevision"] }

// refreshToRevisionTwo makes an anonymous caller force the refresh of revision 1, which the broker serves as revision 2.
func refreshToRevisionTwo(t *testing.T, fixture *sharedFixture, resolver *projectconfig.CredentialResolver[token]) {
	t.Helper()
	fixture.broker.respondAlways(readyReply(2, "second", fixture.clock.Now(), time.Hour))
	value, err := resolver.ResolveAfterRejection(context.Background(), forbiddenDriver{t})
	require.NoError(t, err)
	require.Equal(t, "second", value.Access)
}

func TestRejectionOfACallThatUsedAnOlderRevisionRetriesWithTheCachedRevision(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Hour))
	resolver := fixture.resolver(t)
	callA, callB := callContext("call-a"), callContext("call-b")
	for _, ctx := range []context.Context{callA, callB} {
		value, err := resolver.Resolve(ctx)
		require.NoError(t, err)
		require.Equal(t, "first", value.Access)
	}

	// Call B is rejected first and refreshes; the provider invalidated the access token that call A holds.
	fixture.broker.respondAlways(readyReply(2, "second", epoch.Add(5*time.Minute), time.Hour))
	value, err := resolver.ResolveAfterRejection(callB, forbiddenDriver{t})
	require.NoError(t, err)
	require.Equal(t, "second", value.Access)
	require.Equal(t, 2, fixture.broker.requestCount())
	require.Equal(t, float64(1), rejectedRevision(fixture.broker.request(1)))

	// Call A's rejection retries with the newer revision and does not force another refresh.
	value, err = resolver.ResolveAfterRejection(callA, forbiddenDriver{t})
	require.NoError(t, err)
	require.Equal(t, "second", value.Access)
	require.Equal(t, 2, fixture.broker.requestCount(), "revision 2 is good, so no read is needed")

	// Rejected again with revision 2, call A forces a refresh of that revision.
	fixture.broker.respondAlways(readyReply(3, "third", epoch.Add(10*time.Minute), time.Hour))
	value, err = resolver.ResolveAfterRejection(callA, forbiddenDriver{t})
	require.NoError(t, err)
	require.Equal(t, "third", value.Access)
	require.Equal(t, 3, fixture.broker.requestCount())
	require.Equal(t, float64(2), rejectedRevision(fixture.broker.request(2)))
}

func TestRejectionOfARememberedCallForcesTheRevisionItReceived(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Hour))
	resolver := fixture.resolver(t)
	_, err := resolver.Resolve(callContext("call-a"))
	require.NoError(t, err)

	fixture.broker.respondAlways(readyReply(2, "second", epoch.Add(5*time.Minute), time.Hour))
	value, err := resolver.ResolveAfterRejection(callContext("call-a"), forbiddenDriver{t})
	require.NoError(t, err)
	require.Equal(t, "second", value.Access)
	require.Equal(t, float64(1), rejectedRevision(fixture.broker.request(1)))
}

func TestRejectionOfAnUnknownCallAssumesTheCachedRevision(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Hour))
	resolver := fixture.resolver(t)
	refreshToRevisionTwo(t, fixture, resolver)
	require.Equal(t, float64(1), rejectedRevision(fixture.broker.request(1)))

	// Nothing says which revision this caller used, so the cached revision 2 is the one forced, and it is unchanged.
	_, err := resolver.ResolveAfterRejection(context.Background(), forbiddenDriver{t})
	require.ErrorIs(t, err, projectconfig.ErrRejectedCredentialNotRefreshed)
	require.Equal(t, float64(2), rejectedRevision(fixture.broker.request(2)))
}

func TestRememberedCallsAreBoundedByCount(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Hour))
	resolver := fixture.resolver(t)
	const remembered = 4096
	for index := range remembered + 1 {
		_, err := resolver.Resolve(callContext("call-" + strconv.Itoa(index)))
		require.NoError(t, err)
	}
	refreshToRevisionTwo(t, fixture, resolver)
	requests := fixture.broker.requestCount()

	value, err := resolver.ResolveAfterRejection(callContext("call-"+strconv.Itoa(remembered)), forbiddenDriver{t})
	require.NoError(t, err)
	require.Equal(t, "second", value.Access)
	require.Equal(t, requests, fixture.broker.requestCount(), "a remembered call is served without a read")

	// The first call was forgotten, so its rejection forces the cached revision 2, which the broker does not refresh.
	_, err = resolver.ResolveAfterRejection(callContext("call-0"), forbiddenDriver{t})
	require.ErrorIs(t, err, projectconfig.ErrRejectedCredentialNotRefreshed)
	require.Equal(t, requests+1, fixture.broker.requestCount())
	require.Equal(t, float64(2), rejectedRevision(fixture.broker.request(requests)))
}

func TestRememberedCallsExpireAfterTenMinutes(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Hour))
	resolver := fixture.resolver(t)
	for _, callID := range []string{"call-inside", "call-outside"} {
		_, err := resolver.Resolve(callContext(callID))
		require.NoError(t, err)
	}
	fixture.clock.Advance(10*time.Minute - time.Nanosecond)
	refreshToRevisionTwo(t, fixture, resolver)
	requests := fixture.broker.requestCount()

	value, err := resolver.ResolveAfterRejection(callContext("call-inside"), forbiddenDriver{t})
	require.NoError(t, err)
	require.Equal(t, "second", value.Access, "just inside the retention the call is still remembered")
	require.Equal(t, requests, fixture.broker.requestCount())

	fixture.clock.Advance(time.Nanosecond)
	_, err = resolver.ResolveAfterRejection(callContext("call-outside"), forbiddenDriver{t})
	require.ErrorIs(t, err, projectconfig.ErrRejectedCredentialNotRefreshed, "a forgotten call falls back to the cached revision")
	require.Equal(t, requests+1, fixture.broker.requestCount())
	require.Equal(t, float64(2), rejectedRevision(fixture.broker.request(requests)))
}

func TestRejectionOfACallThatUsedAnotherSelectionServesTheCurrentSelection(t *testing.T) {
	fixture := newPreviewFixture(t, readyReply(1, "first", epoch, time.Minute))
	resolver := fixture.resolver(t)
	_, err := resolver.Resolve(callContext("call-a"))
	require.NoError(t, err)
	fixture.clock.Advance(50 * time.Second)
	fixture.writeConfiguration(t, 1, "conn-9")
	fixture.broker.respondWith(func(request brokerRequest) brokerReply {
		if request.body["sharedConnectionId"] == "conn-9" {
			return readyReply(1, "other", fixture.clock.Now(), time.Hour)
		}
		return statusReply("CONNECTION_NOT_SELECTED")
	})
	value, err := resolver.Resolve(callContext("call-b"))
	require.NoError(t, err)
	require.Equal(t, "other", value.Access)
	requests := fixture.broker.requestCount()

	value, err = resolver.ResolveAfterRejection(callContext("call-a"), forbiddenDriver{t})
	require.NoError(t, err)
	require.Equal(t, "other", value.Access)
	require.Equal(t, requests, fixture.broker.requestCount(), "revision 1 of conn-7 is not revision 1 of conn-9")
}
