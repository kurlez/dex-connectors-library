// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package projectconfig_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/superdurable/dex-connectors-library/sdkgo/internal/testsupport"
	"github.com/superdurable/dex-connectors-library/sdkgo/projectconfig"
	"github.com/superdurable/dex-connectors-library/sdkgo/projectconfig/projectconfigtest"
)

func TestRegisterRelayedTriggerPostsTheSharedConnectionToTheBrokerOrigin(t *testing.T) {
	fixture := newUnreadSharedFixture(t, sharedScope, staticReply(1, sharedAccessOne))

	require.NoError(t, fixture.project.RegisterRelayedTrigger(context.Background(), sharedKey, registrationPath))

	require.Zero(t, fixture.broker.requestCount(), "registration never reads a credential")
	require.Equal(t, 1, fixture.registrar.requestCount())
	request := fixture.registrar.request(0)
	require.Equal(t, http.MethodPost, request.method)
	require.Equal(t, registrationPath, request.path, "the path replaces the read endpoint's path on the same origin")
	require.Equal(t, "Bearer "+scopeToken, request.authorization)
	require.Equal(t, "application/json", request.contentType)
	require.Equal(t, map[string]any{"sharedConnectionId": "conn-7", "connectionName": "releases"}, request.body,
		"the request names no address")
}

func TestRegisterRelayedTriggerClassifiesEachAnswer(t *testing.T) {
	for _, test := range []struct {
		name     string
		reply    brokerReply
		expected error
	}{
		{name: "connection not selected", reply: statusReply("CONNECTION_NOT_SELECTED"), expected: projectconfig.ErrConnectionNotSelected},
		{name: "not relayed", reply: statusReply("NOT_RELAYED"), expected: projectconfig.ErrRelayedTriggerNotRelayed},
		{name: "unknown status", reply: statusReply("PENDING"), expected: projectconfig.ErrCredentialBrokerRejected},
		{name: "not JSON", reply: brokerReply{raw: "registered"}, expected: projectconfig.ErrCredentialBrokerRejected},
		{name: "malformed request", reply: brokerReply{statusCode: http.StatusBadRequest}, expected: projectconfig.ErrCredentialBrokerRejected},
		{name: "too many connection names", reply: brokerReply{statusCode: http.StatusConflict}, expected: projectconfig.ErrCredentialBrokerRejected},
		{name: "request too large", reply: brokerReply{statusCode: http.StatusRequestEntityTooLarge}, expected: projectconfig.ErrCredentialBrokerRejected},
		{name: "redirect", reply: brokerReply{redirectTo: "https://elsewhere.example.test/register"}, expected: projectconfig.ErrCredentialBrokerRejected},
		{name: "overloaded", reply: brokerReply{statusCode: http.StatusTooManyRequests}, expected: projectconfig.ErrCredentialBrokerUnavailable},
		{name: "unavailable", reply: brokerReply{statusCode: http.StatusServiceUnavailable}, expected: projectconfig.ErrCredentialBrokerUnavailable},
		{name: "server failure", reply: brokerReply{statusCode: http.StatusInternalServerError}, expected: projectconfig.ErrCredentialBrokerUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newUnreadSharedFixture(t, sharedScope, staticReply(1, sharedAccessOne))
			fixture.registrar.respondAlways(test.reply)
			err := fixture.project.RegisterRelayedTrigger(context.Background(), sharedKey, registrationPath)
			require.ErrorIs(t, err, test.expected)
			require.NotContains(t, err.Error(), scopeToken)
		})
	}
}

func TestRegisterRelayedTriggerRenewsARefusedScopeTokenOnce(t *testing.T) {
	fixture := newUnreadSharedFixture(t, sharedScope, staticReply(1, sharedAccessOne))
	fixture.rotateScopeToken(t, "rotated-scope-token")
	fixture.registrar.respondWith(func(request brokerRequest) brokerReply {
		if request.authorization != "Bearer rotated-scope-token" {
			return brokerReply{statusCode: http.StatusUnauthorized}
		}
		return statusReply("REGISTERED")
	})

	require.NoError(t, fixture.project.RegisterRelayedTrigger(context.Background(), sharedKey, registrationPath))
	require.Equal(t, 2, fixture.registrar.requestCount())

	fixture.registrar.respondAlways(brokerReply{statusCode: http.StatusUnauthorized})
	err := fixture.project.RegisterRelayedTrigger(context.Background(), sharedKey, registrationPath)
	require.ErrorIs(t, err, projectconfig.ErrCredentialBrokerRejected, "a 401 that renewal does not change is a rejection")
	require.Equal(t, 3, fixture.registrar.requestCount(), "an unchanged token is not sent again")
}

func TestRegisterRelayedTriggerRefusesWithoutARequest(t *testing.T) {
	fixture := newUnreadSharedFixture(t, sharedScope, staticReply(1, sharedAccessOne))
	fixture.allowsStoredConnections = true
	for name, call := range map[string]func() error{
		"ordinary entry": func() error {
			return fixture.project.RegisterRelayedTrigger(context.Background(), ordinaryKey, registrationPath)
		},
		"unknown entry": func() error {
			return fixture.project.RegisterRelayedTrigger(context.Background(), projectconfig.ConnectionKey{ConnectorID: "github", ConnectionName: "missing"}, registrationPath)
		},
		"relative path": func() error {
			return fixture.project.RegisterRelayedTrigger(context.Background(), sharedKey, "register")
		},
		"network-path reference": func() error {
			return fixture.project.RegisterRelayedTrigger(context.Background(), sharedKey, "//elsewhere.example.test/register")
		},
		"path with a query": func() error {
			return fixture.project.RegisterRelayedTrigger(context.Background(), sharedKey, registrationPath+"?to=x")
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, call())
			require.Zero(t, fixture.registrar.requestCount())
		})
	}
}

func TestRelayedTriggerRequiresACredentialBroker(t *testing.T) {
	objects := testsupport.NewObjectStore()
	configurations, err := projectconfig.NewConfigurationStore(&projectconfig.ConfigurationStoreConfig{Objects: objects, Scope: sharedScope})
	require.NoError(t, err)
	_, _, err = configurations.WriteConfiguration(context.Background(), 0, projectconfig.Configuration{
		Connections: []projectconfig.ConnectionConfiguration{ordinaryEntry("local")},
	})
	require.NoError(t, err)
	reference, err := configurations.FreezeConfiguration(context.Background(), 1)
	require.NoError(t, err)
	project, err := projectconfig.LoadProjectForTest(context.Background(), objects, sharedScope, reference, nil, nil)
	require.NoError(t, err)

	require.Error(t, project.RegisterRelayedTrigger(context.Background(), ordinaryKey, registrationPath))
	header := signedRelayHeader(scopeToken, epoch, []byte(`{}`))
	err = project.VerifyRelayedRequest(context.Background(), header, []byte(`{}`), epoch)
	require.Error(t, err)
	require.NotErrorIs(t, err, projectconfig.ErrCredentialBrokerUnavailable, "a missing broker is not retryable")
}

func TestVerifyRelayedRequestAcceptsOnlyAFreshSignatureOverTheRawBody(t *testing.T) {
	fixture := newUnreadSharedFixture(t, sharedScope, staticReply(1, sharedAccessOne))
	body := []byte(`{"event_id":"Ev1","event":{"type":"message"}}`)
	valid := signedRelayHeader(scopeToken, epoch, body)

	require.NoError(t, fixture.project.VerifyRelayedRequest(context.Background(), valid, body, epoch))
	require.NoError(t, fixture.project.VerifyRelayedRequest(context.Background(), valid, body, epoch.Add(5*time.Minute)))
	require.NoError(t, fixture.project.VerifyRelayedRequest(context.Background(), valid, body, epoch.Add(-5*time.Minute)))

	for name, test := range map[string]struct {
		header     http.Header
		body       []byte
		receivedAt time.Time
	}{
		"changed body":          {header: valid, body: []byte(`{"event_id":"Ev2","event":{"type":"message"}}`), receivedAt: epoch},
		"another key":           {header: signedRelayHeader("another-scope-token", epoch, body), body: body, receivedAt: epoch},
		"stale timestamp":       {header: valid, body: body, receivedAt: epoch.Add(5*time.Minute + time.Second)},
		"future timestamp":      {header: valid, body: body, receivedAt: epoch.Add(-5*time.Minute - time.Second)},
		"missing signature":     {header: withoutHeader(valid, projectconfig.RelaySignatureHeader), body: body, receivedAt: epoch},
		"missing timestamp":     {header: withoutHeader(valid, projectconfig.RelayTimestampHeader), body: body, receivedAt: epoch},
		"other version":         {header: withHeader(valid, projectconfig.RelaySignatureHeader, strings.Replace(valid.Get(projectconfig.RelaySignatureHeader), "v1=", "v0=", 1)), body: body, receivedAt: epoch},
		"non-hex signature":     {header: withHeader(valid, projectconfig.RelaySignatureHeader, "v1=zz"), body: body, receivedAt: epoch},
		"truncated signature":   {header: withHeader(valid, projectconfig.RelaySignatureHeader, valid.Get(projectconfig.RelaySignatureHeader)[:20]), body: body, receivedAt: epoch},
		"non-numeric timestamp": {header: withHeader(valid, projectconfig.RelayTimestampHeader, "yesterday"), body: body, receivedAt: epoch},
	} {
		t.Run(name, func(t *testing.T) {
			err := fixture.project.VerifyRelayedRequest(context.Background(), test.header, test.body, test.receivedAt)
			require.ErrorIs(t, err, projectconfig.ErrRelaySignatureInvalid)
			require.NotContains(t, err.Error(), scopeToken)
		})
	}
	require.Zero(t, fixture.broker.requestCount()+fixture.registrar.requestCount(), "verification never calls the broker")
}

func TestVerifyRelayedRequestRereadsARotatedTokenAtMostEveryThirtySeconds(t *testing.T) {
	fixture := newUnreadSharedFixture(t, sharedScope, staticReply(1, sharedAccessOne))
	body := []byte(`{"event_id":"Ev1"}`)
	require.NoError(t, fixture.project.VerifyRelayedRequest(context.Background(), signedRelayHeader(scopeToken, epoch, body), body, epoch))

	fixture.rotateScopeToken(t, "rotated-scope-token")
	fixture.objects.reset()
	rotated := signedRelayHeader("rotated-scope-token", epoch, body)
	require.NoError(t, fixture.project.VerifyRelayedRequest(context.Background(), rotated, body, epoch),
		"a mismatch rereads the head once and verifies with the rotated token")
	require.Equal(t, 1, fixture.objects.currentHeadReads())

	forged := signedRelayHeader("forged-key", epoch, body)
	for range 20 {
		require.ErrorIs(t, fixture.project.VerifyRelayedRequest(context.Background(), forged, body, epoch), projectconfig.ErrRelaySignatureInvalid)
	}
	require.Equal(t, 1, fixture.objects.currentHeadReads(), "wrong signatures within the interval of the last reread read nothing")

	fixture.rotateScopeToken(t, "second-rotation")
	fixture.objects.reset()
	second := signedRelayHeader("second-rotation", epoch, body)
	require.ErrorIs(t, fixture.project.VerifyRelayedRequest(context.Background(), second, body, epoch), projectconfig.ErrRelaySignatureInvalid,
		"within the interval a rotation waits for the next reread")
	require.Zero(t, fixture.objects.currentHeadReads())
	fixture.clock.Advance(30 * time.Second)
	require.NoError(t, fixture.project.VerifyRelayedRequest(context.Background(), second, body, epoch.Add(30*time.Second)))
	require.NoError(t, fixture.project.VerifyRelayedRequest(context.Background(), second, body, epoch.Add(30*time.Second)))
	require.Equal(t, 1, fixture.objects.currentHeadReads(), "the renewed token is cached")
}

func signedRelayHeader(token string, signedAt time.Time, body []byte) http.Header {
	timestamp := strconv.FormatInt(signedAt.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte("v1:" + timestamp + ":"))
	_, _ = mac.Write(body)
	header := http.Header{}
	header.Set(projectconfig.RelayTimestampHeader, timestamp)
	header.Set(projectconfig.RelaySignatureHeader, "v1="+hex.EncodeToString(mac.Sum(nil)))
	return header
}

func withHeader(header http.Header, name string, value string) http.Header {
	changed := header.Clone()
	changed.Set(name, value)
	return changed
}

func withoutHeader(header http.Header, name string) http.Header {
	changed := header.Clone()
	changed.Del(name)
	return changed
}

func TestProjectConfigTestLoadsAProjectThatServesRelayedTriggers(t *testing.T) {
	fixture := newUnreadSharedFixture(t, sharedScope, staticReply(1, sharedAccessOne))
	reference := fixture.writeConfiguration(t, 1, "conn-7")
	project, err := projectconfigtest.LoadProject(context.Background(), fixture.objects, sharedScope, reference)
	require.NoError(t, err)
	require.Equal(t, reference, project.Snapshot)

	require.NoError(t, project.RegisterRelayedTrigger(context.Background(), sharedKey, registrationPath))
	body := []byte(`{"event_id":"Ev1"}`)
	now := time.Now()
	require.NoError(t, project.VerifyRelayedRequest(context.Background(), signedRelayHeader(scopeToken, now, body), body, now))

	_, err = projectconfigtest.LoadProject(context.Background(), nil, sharedScope, reference)
	require.Error(t, err)
}

func TestSignRelayedRequestProducesWhatVerifyRelayedRequestAccepts(t *testing.T) {
	fixture := newUnreadSharedFixture(t, sharedScope, staticReply(1, sharedAccessOne))
	body := []byte(`{"event_id":"Ev1"}`)
	signature := projectconfig.SignRelayedRequest(scopeToken, epoch, body)
	require.Equal(t, signedRelayHeader(scopeToken, epoch, body).Get(projectconfig.RelaySignatureHeader), signature)

	header := http.Header{}
	header.Set(projectconfig.RelayTimestampHeader, strconv.FormatInt(epoch.Unix(), 10))
	header.Set(projectconfig.RelaySignatureHeader, signature)
	require.NoError(t, fixture.project.VerifyRelayedRequest(context.Background(), header, body, epoch))

	require.ErrorIs(t, fixture.project.VerifyRelayedRequest(context.Background(), header, []byte(`{"event_id":"Ev2"}`), epoch), projectconfig.ErrRelaySignatureInvalid)
	header.Set(projectconfig.RelaySignatureHeader, projectconfig.SignRelayedRequest("another-scope-token", epoch, body))
	require.ErrorIs(t, fixture.project.VerifyRelayedRequest(context.Background(), header, body, epoch), projectconfig.ErrRelaySignatureInvalid)
}
