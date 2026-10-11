// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package projectconfig_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/superdurable/dex-connectors-library/sdkgo/projectconfig"
)

func requireNoSecrets(t *testing.T, err error) {
	t.Helper()
	for _, secret := range []string{scopeToken, sharedAccessOne, "refresh-secret"} {
		require.NotContains(t, err.Error(), secret)
	}
}

func TestLoadTakesSettingsFromTheEntryAndTheBrokerIsReadOnFirstUse(t *testing.T) {
	fixture := newUnreadSharedFixture(t, sharedScope, readyReply(1, sharedAccessOne, epoch, time.Hour))

	var shared, ordinary githubSettings
	require.NoError(t, fixture.project.Configuration.DecodeConnectionConfiguration(sharedKey, &shared))
	require.NoError(t, fixture.project.Configuration.DecodeConnectionConfiguration(ordinaryKey, &ordinary))
	require.Equal(t, 8192, shared.MaxResponseBytes, "a shared entry's settings are frozen in the document")
	require.Equal(t, 4096, ordinary.MaxResponseBytes)
	require.Zero(t, fixture.broker.requestCount())

	value, err := fixture.resolver(t).ResolveWithRefresh(context.Background(), forbiddenDriver{t})
	require.NoError(t, err)
	require.Equal(t, token{Access: sharedAccessOne, Refresh: "refresh-secret"}, value)
	require.Equal(t, 1, fixture.broker.requestCount())
	request := fixture.broker.request(0)
	require.Equal(t, http.MethodPost, request.method)
	require.Equal(t, brokerPath, request.path)
	require.Equal(t, "Bearer "+scopeToken, request.authorization)
	require.Equal(t, "application/json", request.contentType)
	require.Equal(t, map[string]any{"sharedConnectionId": "conn-7"}, request.body)

	_, err = fixture.resolver(t).Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, fixture.broker.requestCount(), "the credential is cached")
}

func TestOrdinaryEntriesKeepTheirStoredConnections(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, sharedAccessOne, epoch, time.Hour))
	fixture.allowsStoredConnections = true
	store, err := projectconfig.NewConnectionStore(&projectconfig.ConnectionStoreConfig{Objects: fixture.objects, Scope: sharedScope})
	require.NoError(t, err)
	expiresAt := time.Now().Add(time.Hour)
	_, err = store.ReplaceCredential(context.Background(), ordinaryKey, 0, projectconfig.CredentialMaterial{Credentials: []byte(`{"access":"stored"}`), ExpiresAt: &expiresAt, AuthMethod: "default"})
	require.NoError(t, err)

	resolver, err := projectconfig.NewCredentialResolver(fixture.project.Connections, ordinaryKey, decodeToken, encodeToken)
	require.NoError(t, err)
	value, err := resolver.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "stored", value.Access)
	require.Equal(t, 1, fixture.broker.requestCount(), "only the shared entry reads the broker")
}

func TestSharedCredentialIsReadAgainAtTheProportionalSkew(t *testing.T) {
	for name, test := range map[string]struct {
		lifetime, skew time.Duration
	}{
		"a 60-second token is read again 15 seconds before expiry": {lifetime: time.Minute, skew: 15 * time.Second},
		"a one-hour token is read again 5 minutes before expiry":   {lifetime: time.Hour, skew: 5 * time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newSharedFixture(t, readyReply(1, "first", epoch, test.lifetime))
			resolver := fixture.resolver(t)
			ctx := context.Background()

			fixture.clock.Advance(test.lifetime - test.skew - time.Nanosecond)
			value, err := resolver.ResolveWithRefresh(ctx, forbiddenDriver{t})
			require.NoError(t, err)
			require.Equal(t, "first", value.Access)
			require.Equal(t, 1, fixture.broker.requestCount(), "no read before the skew")

			fixture.clock.Advance(time.Nanosecond)
			reissuedAt := fixture.clock.Now()
			fixture.broker.respondAlways(readyReply(2, "second", reissuedAt, test.lifetime))
			value, err = resolver.ResolveWithRefresh(ctx, forbiddenDriver{t})
			require.NoError(t, err)
			require.Equal(t, "second", value.Access)
			require.Equal(t, 2, fixture.broker.requestCount())
			require.NotContains(t, fixture.broker.request(1).body, "rejectedRevision", "a skew read is not a rejection")

			value, err = resolver.Resolve(ctx)
			require.NoError(t, err)
			require.Equal(t, "second", value.Access)
			require.Equal(t, 2, fixture.broker.requestCount(), "the new credential is fresh")
		})
	}
}

func TestStaleCredentialThatTheBrokerReturnsUnchangedIsNotReadOnEveryCall(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Minute))
	resolver := fixture.resolver(t)
	fixture.clock.Advance(50 * time.Second)

	for range 5 {
		value, err := resolver.Resolve(context.Background())
		require.NoError(t, err)
		require.Equal(t, "first", value.Access)
	}
	require.Equal(t, 2, fixture.broker.requestCount(), "the load and one read; the others wait for the suppression interval")

	fixture.clock.Advance(time.Second)
	_, err := resolver.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, 3, fixture.broker.requestCount())
}

func TestStaticSharedCredentialIsReadAgainEveryFifteenMinutes(t *testing.T) {
	fixture := newSharedFixture(t, staticReply(1, "key-1"))
	resolver := fixture.resolver(t)

	fixture.clock.Advance(15*time.Minute - time.Nanosecond)
	value, err := resolver.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "key-1", value.Access)
	require.Equal(t, 1, fixture.broker.requestCount())

	fixture.clock.Advance(time.Nanosecond)
	fixture.broker.respondAlways(staticReply(2, "key-2"))
	value, err = resolver.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "key-2", value.Access, "a replaced API key is picked up")
	require.Equal(t, 2, fixture.broker.requestCount())

	fixture.clock.Advance(14 * time.Minute)
	_, err = resolver.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, fixture.broker.requestCount(), "the interval counts from the latest read")
}

// blockBrokerUntilReleased holds every broker request until the returned function runs, and at the latest until cleanup.
func blockBrokerUntilReleased(t *testing.T, fixture *sharedFixture, reply func() brokerReply) (release func()) {
	t.Helper()
	released := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(released) }) }
	t.Cleanup(release)
	fixture.broker.respondWith(func(brokerRequest) brokerReply {
		<-released
		return reply()
	})
	return release
}

func TestCallersDoNotWaitForAReadInFlightWhileTheStaleCredentialIsUnexpired(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Minute))
	resolver := fixture.resolver(t)
	fixture.clock.Advance(50 * time.Second)
	release := blockBrokerUntilReleased(t, fixture, func() brokerReply { return readyReply(2, "second", fixture.clock.Now(), time.Minute) })
	leader := make(chan token, 1)
	go func() {
		value, err := resolver.Resolve(context.Background())
		if err != nil {
			t.Errorf("leader resolve failed: %v", err)
		}
		leader <- value
	}()
	require.Eventually(t, func() bool { return fixture.broker.requestCount() >= 2 }, 5*time.Second, 5*time.Millisecond)

	for range 16 {
		value, err := resolver.ResolveWithRefresh(context.Background(), forbiddenDriver{t})
		require.NoError(t, err)
		require.Equal(t, "first", value.Access, "the stale credential has not expired, so nobody waits for the read")
	}
	require.Equal(t, 2, fixture.broker.requestCount(), "the load and the leader's read")

	release()
	require.Equal(t, "second", (<-leader).Access)
}

func TestConcurrentResolvesOfAnExpiredCredentialShareOneBrokerRead(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Minute))
	resolver := fixture.resolver(t)
	fixture.clock.Advance(61 * time.Second)
	release := blockBrokerUntilReleased(t, fixture, func() brokerReply { return readyReply(2, "second", fixture.clock.Now(), time.Minute) })

	const callers = 16
	values := make(chan token, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			value, err := resolver.ResolveWithRefresh(context.Background(), forbiddenDriver{t})
			if err != nil {
				t.Errorf("resolve failed: %v", err)
			}
			values <- value
		}()
	}
	require.Eventually(t, func() bool { return fixture.broker.requestCount() >= 2 }, 5*time.Second, 5*time.Millisecond)
	require.Never(t, func() bool { return fixture.broker.requestCount() > 2 }, 200*time.Millisecond, 10*time.Millisecond)
	release()
	wait.Wait()
	close(values)

	for value := range values {
		require.Equal(t, "second", value.Access)
	}
	require.Equal(t, 2, fixture.broker.requestCount(), "the load and one shared read")
}

func TestCallerWaitingForABrokerReadStopsWhenItsContextEnds(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Minute))
	resolver := fixture.resolver(t)
	fixture.clock.Advance(61 * time.Second)
	release := blockBrokerUntilReleased(t, fixture, func() brokerReply { return readyReply(2, "second", fixture.clock.Now(), time.Minute) })
	leader := make(chan token, 1)
	go func() {
		value, err := resolver.Resolve(context.Background())
		if err != nil {
			t.Errorf("leader resolve failed: %v", err)
		}
		leader <- value
	}()
	require.Eventually(t, func() bool { return fixture.broker.requestCount() >= 2 }, 5*time.Second, 5*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := resolver.Resolve(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	release()
	require.Equal(t, "second", (<-leader).Access)
	require.Equal(t, 2, fixture.broker.requestCount())
}

func TestRejectionReadsAgainWithTheRejectedRevision(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Hour))
	resolver := fixture.resolver(t)
	fixture.broker.respondAlways(readyReply(2, "second", epoch.Add(2*time.Minute), time.Hour))

	value, err := resolver.ResolveAfterRejection(context.Background(), forbiddenDriver{t})
	require.NoError(t, err)
	require.Equal(t, "second", value.Access)
	require.Equal(t, 2, fixture.broker.requestCount())
	require.Equal(t, map[string]any{"sharedConnectionId": "conn-7", "rejectedRevision": float64(1)}, fixture.broker.request(1).body)

	value, err = resolver.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "second", value.Access)
	require.Equal(t, 2, fixture.broker.requestCount(), "the replacement is cached")
}

func TestRejectionOfAnUnchangedRevisionFailsTheCallInsteadOfLooping(t *testing.T) {
	for name, reply := range map[string]brokerReply{
		"a credential within the forced-refresh floor": readyReply(1, "first", epoch, time.Hour),
		"a static credential":                          staticReply(1, "first"),
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newSharedFixture(t, reply)
			resolver := fixture.resolver(t)

			_, err := resolver.ResolveAfterRejection(context.Background(), forbiddenDriver{t})
			require.ErrorIs(t, err, projectconfig.ErrRejectedCredentialNotRefreshed)
			require.NotErrorIs(t, err, projectconfig.ErrReauthorizationRequired)
			require.Equal(t, 2, fixture.broker.requestCount(), "exactly one extra read, with no retry")
			require.Equal(t, map[string]any{"sharedConnectionId": "conn-7", "rejectedRevision": float64(1)}, fixture.broker.request(1).body)

			value, err := resolver.Resolve(context.Background())
			require.NoError(t, err)
			require.Equal(t, "first", value.Access, "the cached credential stays for later calls")
			require.Equal(t, 2, fixture.broker.requestCount())
		})
	}
}

func TestRejectionWhileTheBrokerIsUnavailableDoesNotReuseTheRejectedCredential(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Hour))
	fixture.broker.respondAlways(brokerReply{statusCode: http.StatusServiceUnavailable, raw: "{}"})

	_, err := fixture.resolver(t).ResolveAfterRejection(context.Background(), forbiddenDriver{t})
	require.ErrorIs(t, err, projectconfig.ErrCredentialBrokerUnavailable)
}

func TestReauthorizationRequiredIsReportedAndClearsTheCache(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Minute))
	resolver := fixture.resolver(t)
	fixture.clock.Advance(50 * time.Second)
	fixture.broker.respondAlways(statusReply("REAUTHORIZATION_REQUIRED"))

	_, err := resolver.ResolveWithRefresh(context.Background(), forbiddenDriver{t})
	require.ErrorIs(t, err, projectconfig.ErrReauthorizationRequired)

	// Callers that arrive right after share the answer; later ones ask again and find the reauthorized credential.
	_, err = resolver.Resolve(context.Background())
	require.ErrorIs(t, err, projectconfig.ErrReauthorizationRequired)
	require.Equal(t, 2, fixture.broker.requestCount())
	fixture.clock.Advance(2 * time.Second)
	fixture.broker.respondAlways(readyReply(2, "second", fixture.clock.Now(), time.Hour))
	value, err := resolver.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "second", value.Access)
}

func TestConnectionNotSelectedRereadsTheConfigurationDocumentOnceThenFails(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Minute))
	resolver := fixture.resolver(t)
	fixture.clock.Advance(50 * time.Second)
	fixture.broker.respondAlways(statusReply("CONNECTION_NOT_SELECTED"))
	fixture.objects.reset()

	_, err := resolver.Resolve(context.Background())
	require.ErrorIs(t, err, projectconfig.ErrConnectionNotSelected)
	require.Equal(t, 1, fixture.objects.currentHeadReads(), "the document is reread exactly once")
	require.Equal(t, 2, fixture.broker.requestCount(), "the same selection is not read again")
}

func TestConnectionNotSelectedFollowsTheSelectionOfTheRereadDocument(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Minute))
	resolver := fixture.resolver(t)
	fixture.clock.Advance(50 * time.Second)
	fixture.writeConfiguration(t, 1, "conn-9")
	fixture.broker.respondWith(func(request brokerRequest) brokerReply {
		if request.body["sharedConnectionId"] == "conn-9" {
			return readyReply(5, "other", fixture.clock.Now(), time.Hour)
		}
		return statusReply("CONNECTION_NOT_SELECTED")
	})
	fixture.objects.reset()

	value, err := resolver.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "other", value.Access)
	require.Equal(t, 1, fixture.objects.currentHeadReads())
	require.Equal(t, 3, fixture.broker.requestCount())
	require.Equal(t, map[string]any{"sharedConnectionId": "conn-9"}, fixture.broker.request(2).body)

	value, err = resolver.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "other", value.Access)
	require.Equal(t, 3, fixture.broker.requestCount(), "the new selection is cached")
}

func TestConnectionNotSelectedAgainAfterTheNewSelectionFailsWithoutAnotherReread(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Minute))
	resolver := fixture.resolver(t)
	fixture.clock.Advance(50 * time.Second)
	fixture.writeConfiguration(t, 1, "conn-9")
	fixture.broker.respondAlways(statusReply("CONNECTION_NOT_SELECTED"))
	fixture.objects.reset()

	_, err := resolver.Resolve(context.Background())
	require.ErrorIs(t, err, projectconfig.ErrConnectionNotSelected)
	require.Equal(t, 1, fixture.objects.currentHeadReads())
	require.Equal(t, 3, fixture.broker.requestCount(), "the load, the old selection and the new one")
}

func TestConnectionNotSelectedFailsWhenTheRereadDocumentNoLongerHasTheEntry(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Minute))
	resolver := fixture.resolver(t)
	fixture.clock.Advance(50 * time.Second)
	_, _, err := fixture.configurations.WriteConfiguration(context.Background(), 1, projectconfig.Configuration{Connections: []projectconfig.ConnectionConfiguration{ordinaryEntry("local")}})
	require.NoError(t, err)
	fixture.broker.respondAlways(statusReply("CONNECTION_NOT_SELECTED"))

	_, err = resolver.Resolve(context.Background())
	require.ErrorIs(t, err, projectconfig.ErrConnectionNotSelected)
	require.Equal(t, 2, fixture.broker.requestCount())
}

func TestChangedSelectionNeverFallsBackToTheOldConnectionsCredential(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Minute))
	resolver := fixture.resolver(t)
	fixture.clock.Advance(50 * time.Second)
	fixture.writeConfiguration(t, 1, "conn-9")
	fixture.broker.respondWith(func(request brokerRequest) brokerReply {
		if request.body["sharedConnectionId"] == "conn-9" {
			return brokerReply{statusCode: http.StatusServiceUnavailable, raw: "{}"}
		}
		return statusReply("CONNECTION_NOT_SELECTED")
	})

	_, err := resolver.Resolve(context.Background())
	require.ErrorIs(t, err, projectconfig.ErrCredentialBrokerUnavailable, "the old connection's credential has not expired but is no longer this entry's")

	fixture.clock.Advance(2 * time.Second)
	fixture.broker.respondAlways(readyReply(5, "other", fixture.clock.Now(), time.Hour))
	value, err := resolver.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "other", value.Access)
	require.Equal(t, map[string]any{"sharedConnectionId": "conn-9"}, fixture.broker.request(fixture.broker.requestCount()-1).body)
}

func TestCachedCredentialKeepsWorkingWhileTheBrokerIsUnavailable(t *testing.T) {
	for name, outage := range map[string]brokerReply{
		"HTTP 503":                {statusCode: http.StatusServiceUnavailable, raw: "{}"},
		"HTTP 429":                {statusCode: http.StatusTooManyRequests, raw: "{}"},
		"HTTP 408":                {statusCode: http.StatusRequestTimeout, raw: "{}"},
		"an already expired read": readyReply(2, "late", epoch.Add(-time.Hour), time.Minute),
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Minute))
			resolver := fixture.resolver(t)
			fixture.broker.respondAlways(outage)
			ctx := context.Background()

			fixture.clock.Advance(46 * time.Second)
			value, err := resolver.Resolve(ctx)
			require.NoError(t, err)
			require.Equal(t, "first", value.Access, "the credential has not expired")
			require.Equal(t, 2, fixture.broker.requestCount())

			_, err = resolver.Resolve(ctx)
			require.NoError(t, err)
			require.Equal(t, 2, fixture.broker.requestCount(), "the broker is not asked again right away")

			fixture.clock.Advance(10 * time.Second)
			value, err = resolver.Resolve(ctx)
			require.NoError(t, err)
			require.Equal(t, "first", value.Access)
			require.Equal(t, 3, fixture.broker.requestCount(), "the broker is asked again after the retry interval")

			fixture.clock.Advance(5 * time.Second)
			_, err = resolver.Resolve(ctx)
			require.ErrorIs(t, err, projectconfig.ErrCredentialBrokerUnavailable, "an expired credential is never returned")
			requireNoSecrets(t, err)
			require.Equal(t, 4, fixture.broker.requestCount())

			_, err = resolver.Resolve(ctx)
			require.ErrorIs(t, err, projectconfig.ErrCredentialBrokerUnavailable)
			require.Equal(t, 4, fixture.broker.requestCount(), "callers right after a failed read share its failure")

			fixture.clock.Advance(2 * time.Second)
			fixture.broker.respondAlways(readyReply(2, "second", fixture.clock.Now(), time.Hour))
			value, err = resolver.Resolve(ctx)
			require.NoError(t, err)
			require.Equal(t, "second", value.Access, "the broker's recovery is picked up")
		})
	}
}

func TestUnreachableBrokerIsUnavailable(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Minute))
	resolver := fixture.resolver(t)
	fixture.server.Close()

	fixture.clock.Advance(46 * time.Second)
	value, err := resolver.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "first", value.Access)

	fixture.clock.Advance(20 * time.Second)
	_, err = resolver.Resolve(context.Background())
	require.ErrorIs(t, err, projectconfig.ErrCredentialBrokerUnavailable)
	requireNoSecrets(t, err)
}

func TestBrokerRefusalsAndInvalidAnswersAreRejections(t *testing.T) {
	for name, reply := range map[string]brokerReply{
		"an invalid scope token":        {statusCode: http.StatusUnauthorized, body: map[string]string{"error": scopeToken}},
		"a forbidden scope":             {statusCode: http.StatusForbidden, raw: "{}"},
		"a redirect":                    {redirectTo: "/elsewhere"},
		"a body that is not JSON":       {raw: "not json"},
		"an unknown status":             {body: map[string]any{"status": "MAYBE"}},
		"READY without a credential":    {body: map[string]any{"status": "READY", "revision": 2}},
		"READY with a credential array": {body: map[string]any{"status": "READY", "revision": 2, "credential": []string{sharedAccessOne}}},
		"READY without a revision":      {body: map[string]any{"status": "READY", "credential": map[string]string{"access": sharedAccessOne}}},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Minute))
			fixture.clock.Advance(50 * time.Second)
			fixture.broker.respondAlways(reply)

			_, err := fixture.resolver(t).Resolve(context.Background())
			require.ErrorIs(t, err, projectconfig.ErrCredentialBrokerRejected)
			requireNoSecrets(t, err)
			require.Equal(t, 2, fixture.broker.requestCount())
		})
	}
}

func TestApplicationStartsWhileTheBrokerIsUnavailable(t *testing.T) {
	fixture := newUnreadSharedFixture(t, sharedScope, brokerReply{statusCode: http.StatusBadGateway, raw: "{}"})
	var settings githubSettings
	require.NoError(t, fixture.project.Configuration.DecodeConnectionConfiguration(sharedKey, &settings))

	_, err := fixture.resolver(t).Resolve(context.Background())
	require.ErrorIs(t, err, projectconfig.ErrCredentialBrokerUnavailable)
	requireNoSecrets(t, err)

	fixture.clock.Advance(2 * time.Second)
	fixture.broker.respondAlways(readyReply(1, sharedAccessOne, fixture.clock.Now(), time.Hour))
	value, err := fixture.resolver(t).Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, sharedAccessOne, value.Access, "the first read that finds the broker answers")
}

func TestFirstUseReportsAnEntryThatIsNotUsable(t *testing.T) {
	for name, test := range map[string]struct {
		status   string
		expected error
	}{
		"reauthorization required": {status: "REAUTHORIZATION_REQUIRED", expected: projectconfig.ErrReauthorizationRequired},
		"connection not selected":  {status: "CONNECTION_NOT_SELECTED", expected: projectconfig.ErrConnectionNotSelected},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newUnreadSharedFixture(t, sharedScope, statusReply(test.status))
			_, err := fixture.resolver(t).Resolve(context.Background())
			require.ErrorIs(t, err, test.expected)
		})
	}
}

func TestBrokerTokenIsReadOnceFromItsPinnedSecret(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Minute))
	resolver := fixture.resolver(t)
	secretReads := func() int {
		return fixture.objects.countKeys(func(access objectAccess) bool {
			return access.isRead && strings.Contains(access.key, "/app-secrets/DEX_CREDENTIAL_BROKER_TOKEN/")
		})
	}
	require.Equal(t, 1, secretReads())

	for range 3 {
		fixture.clock.Advance(50 * time.Second)
		fixture.broker.respondAlways(readyReply(uint64(fixture.broker.requestCount()+1), "next", fixture.clock.Now(), time.Minute))
		_, err := resolver.Resolve(context.Background())
		require.NoError(t, err)
	}
	require.Equal(t, 4, fixture.broker.requestCount())
	require.Equal(t, 1, secretReads())
	for index := range 4 {
		require.Equal(t, "Bearer "+scopeToken, fixture.broker.request(index).authorization)
	}
}
