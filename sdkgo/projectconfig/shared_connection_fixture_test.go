// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package projectconfig_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/superdurable/dex-connectors-library/sdkgo/internal/testsupport"
	"github.com/superdurable/dex-connectors-library/sdkgo/projectconfig"
)

const (
	brokerPath      = "/internal/connection-credentials/read"
	scopeToken      = "scope-token"
	sharedAccessOne = "access-secret-1"
)

var (
	sharedKey   = projectconfig.ConnectionKey{ConnectorID: "github", ConnectionName: "releases"}
	ordinaryKey = projectconfig.ConnectionKey{ConnectorID: "github", ConnectionName: "local"}
	epoch       = time.Date(2026, time.October, 1, 9, 0, 0, 0, time.UTC)
)

type githubSettings struct {
	MaxResponseBytes int `json:"maxResponseBytes"`
}

// testClock is the clock of the project under test; the broker server and contexts use real time.
type testClock struct {
	mutex sync.Mutex
	now   time.Time
}

func (clock *testClock) Now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.now
}

func (clock *testClock) Advance(duration time.Duration) {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	clock.now = clock.now.Add(duration)
}

// recordingObjectStore records every object access so tests can prove which keys a path touched.
type recordingObjectStore struct {
	inner    *testsupport.ObjectStore
	mutex    sync.Mutex
	accesses []objectAccess
}

type objectAccess struct {
	key, version string
	isRead       bool
}

func (store *recordingObjectStore) ReadObject(ctx context.Context, key, version string) (projectconfig.Object, error) {
	store.record(key, version, true)
	return store.inner.ReadObject(ctx, key, version)
}

func (store *recordingObjectStore) CreateObject(ctx context.Context, key string, contents []byte) (projectconfig.Object, error) {
	store.record(key, "", false)
	return store.inner.CreateObject(ctx, key, contents)
}

func (store *recordingObjectStore) CompareAndSwapObject(ctx context.Context, key, expectedETag string, contents []byte) (projectconfig.Object, error) {
	store.record(key, "", false)
	return store.inner.CompareAndSwapObject(ctx, key, expectedETag, contents)
}

func (store *recordingObjectStore) record(key, version string, isRead bool) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	store.accesses = append(store.accesses, objectAccess{key: key, version: version, isRead: isRead})
}

func (store *recordingObjectStore) reset() {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	store.accesses = nil
}

func (store *recordingObjectStore) countKeys(match func(objectAccess) bool) int {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	count := 0
	for _, access := range store.accesses {
		if match(access) {
			count++
		}
	}
	return count
}

// currentHeadReads counts reads of the configuration head without a pinned version, which only a reread makes.
func (store *recordingObjectStore) currentHeadReads() int {
	return store.countKeys(func(access objectAccess) bool {
		return access.isRead && strings.HasSuffix(access.key, "/configuration/head") && access.version == ""
	})
}

func (store *recordingObjectStore) storedConnectionAccesses() int {
	return store.countKeys(func(access objectAccess) bool { return strings.Contains(access.key, "/connections/") })
}

type brokerRequest struct {
	method, path, authorization, contentType string
	body                                     map[string]any
}

type brokerReply struct {
	statusCode int
	body       any
	raw        string
	redirectTo string
}

// testBroker is the platform's credential read endpoint.
type testBroker struct {
	mutex    sync.Mutex
	requests []brokerRequest
	respond  func(brokerRequest) brokerReply
}

func (broker *testBroker) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	contents, _ := io.ReadAll(request.Body)
	received := brokerRequest{method: request.Method, path: request.URL.Path, authorization: request.Header.Get("Authorization"), contentType: request.Header.Get("Content-Type")}
	_ = json.Unmarshal(contents, &received.body)
	broker.mutex.Lock()
	broker.requests = append(broker.requests, received)
	respond := broker.respond
	broker.mutex.Unlock()
	reply := respond(received)
	if reply.redirectTo != "" {
		http.Redirect(writer, request, reply.redirectTo, http.StatusTemporaryRedirect)
		return
	}
	if reply.statusCode == 0 {
		reply.statusCode = http.StatusOK
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(reply.statusCode)
	if reply.raw != "" {
		_, _ = writer.Write([]byte(reply.raw))
		return
	}
	_ = json.NewEncoder(writer).Encode(reply.body)
}

func (broker *testBroker) respondWith(respond func(brokerRequest) brokerReply) {
	broker.mutex.Lock()
	defer broker.mutex.Unlock()
	broker.respond = respond
}

func (broker *testBroker) respondAlways(reply brokerReply) {
	broker.respondWith(func(brokerRequest) brokerReply { return reply })
}

func (broker *testBroker) requestCount() int {
	broker.mutex.Lock()
	defer broker.mutex.Unlock()
	return len(broker.requests)
}

func (broker *testBroker) request(index int) brokerRequest {
	broker.mutex.Lock()
	defer broker.mutex.Unlock()
	return broker.requests[index]
}

// readyReply is a READY answer for a credential issued at issuedAt that expires after lifetime.
func readyReply(revision uint64, access string, issuedAt time.Time, lifetime time.Duration) brokerReply {
	return brokerReply{body: map[string]any{
		"status": "READY", "credential": map[string]string{"access": access, "refresh": "refresh-secret"}, "revision": revision,
		"issuedAt": issuedAt.Format(time.RFC3339Nano), "expiresAt": issuedAt.Add(lifetime).Format(time.RFC3339Nano),
	}}
}

// staticReply is a READY answer for a credential that never expires, such as an API key.
func staticReply(revision uint64, access string) brokerReply {
	return brokerReply{body: map[string]any{
		"status": "READY", "credential": map[string]string{"access": access},
		"revision": revision,
		"issuedAt": epoch.Format(time.RFC3339Nano),
	}}
}

func statusReply(status string) brokerReply {
	return brokerReply{body: map[string]any{"status": status}}
}

// sharedFixture is a loaded project whose "releases" entry is shared and whose "local" entry is ordinary.
type sharedFixture struct {
	scope          projectconfig.Scope
	clock          *testClock
	objects        *recordingObjectStore
	configurations *projectconfig.ConfigurationStore
	broker         *testBroker
	server         *httptest.Server
	project        *projectconfig.LoadedProject
	// allowsStoredConnections lets a test that exercises an ordinary entry touch connections/*.
	allowsStoredConnections bool
}

// newSharedFixture loads a Live project whose broker serves first, and reads it once so the credential is cached.
func newSharedFixture(t *testing.T, first brokerReply) *sharedFixture {
	t.Helper()
	return warmedSharedFixture(t, sharedScope, first)
}

// newPreviewFixture is newSharedFixture for a Preview project, whose configuration may select another connection.
func newPreviewFixture(t *testing.T, first brokerReply) *sharedFixture {
	t.Helper()
	return warmedSharedFixture(t, previewScope, first)
}

func warmedSharedFixture(t *testing.T, scope projectconfig.Scope, first brokerReply) *sharedFixture {
	t.Helper()
	fixture := newUnreadSharedFixture(t, scope, first)
	_, err := fixture.resolver(t).Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, fixture.broker.requestCount())
	return fixture
}

// newUnreadSharedFixture loads the project and proves that loading does not read the broker.
func newUnreadSharedFixture(t *testing.T, scope projectconfig.Scope, first brokerReply) *sharedFixture {
	t.Helper()
	fixture := &sharedFixture{scope: scope, clock: &testClock{now: epoch}, objects: &recordingObjectStore{inner: testsupport.NewObjectStore()}, broker: &testBroker{}}
	fixture.broker.respondAlways(first)
	mux := http.NewServeMux()
	mux.Handle(brokerPath, fixture.broker)
	fixture.server = httptest.NewServer(mux)
	t.Cleanup(fixture.server.Close)
	t.Cleanup(func() {
		if !fixture.allowsStoredConnections {
			require.Zero(t, fixture.objects.storedConnectionAccesses(), "a shared entry must never read or write connections/*")
		}
	})

	var err error
	fixture.configurations, err = projectconfig.NewConfigurationStore(&projectconfig.ConfigurationStoreConfig{Objects: fixture.objects, Scope: scope})
	require.NoError(t, err)
	reference := fixture.writeConfiguration(t, 0, "conn-7")
	fixture.project, err = projectconfig.LoadProjectForTest(context.Background(), fixture.objects, scope, reference, nil, fixture.clock.Now)
	require.NoError(t, err)
	require.Zero(t, fixture.broker.requestCount(), "loading a project never reads the broker")
	return fixture
}

// writeConfiguration writes the document at expectedRevision, selecting sharedConnectionID for "releases", and freezes it.
func (fixture *sharedFixture) writeConfiguration(t *testing.T, expectedRevision uint64, sharedConnectionID string) projectconfig.SnapshotRef {
	t.Helper()
	broker := credentialBroker(t, fixture.configurations, fixture.server.URL+brokerPath)
	_, _, err := fixture.configurations.WriteConfiguration(context.Background(), expectedRevision, projectconfig.Configuration{
		Connections:      []projectconfig.ConnectionConfiguration{ordinaryEntry("local"), sharedEntry("releases", sharedConnectionID)},
		CredentialBroker: broker,
	})
	require.NoError(t, err)
	reference, err := fixture.configurations.FreezeConfiguration(context.Background(), expectedRevision+1)
	require.NoError(t, err)
	return reference
}

func (fixture *sharedFixture) resolver(t *testing.T) *projectconfig.CredentialResolver[token] {
	t.Helper()
	resolver, err := projectconfig.NewCredentialResolver(fixture.project.Connections, sharedKey, decodeToken, encodeToken)
	require.NoError(t, err)
	return resolver
}

// forbiddenDriver fails the test when a shared entry consults the connector's refresh driver.
type forbiddenDriver struct{ t *testing.T }

func (driver forbiddenDriver) RefreshRequired(projectconfig.RefreshState[token]) bool {
	driver.t.Error("a shared entry must not ask the refresh driver whether to refresh")
	return true
}

func (driver forbiddenDriver) Refresh(context.Context, projectconfig.RefreshState[token]) (projectconfig.RefreshResult[token], error) {
	driver.t.Error("a shared entry must not call the refresh driver")
	return projectconfig.RefreshResult[token]{}, nil
}

// rotateScopeToken writes a new scope token secret and points the configuration head at it.
func (fixture *sharedFixture) rotateScopeToken(t *testing.T, newToken string) {
	t.Helper()
	current, _, err := fixture.configurations.ReadConfiguration(context.Background())
	require.NoError(t, err)
	reference, err := fixture.configurations.CreateCredentialBrokerToken(context.Background(), newToken)
	require.NoError(t, err)
	current.CredentialBroker = &projectconfig.CredentialBrokerConfiguration{Endpoint: current.CredentialBroker.Endpoint, TokenSecretRef: reference}
	_, _, err = fixture.configurations.WriteConfiguration(context.Background(), current.Revision, current)
	require.NoError(t, err)
}
