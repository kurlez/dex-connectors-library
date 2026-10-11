// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package projectconfig_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/superdurable/dex-connectors-library/sdkgo"
	"github.com/superdurable/dex-connectors-library/sdkgo/projectconfig"
	"github.com/superdurable/dex-connectors-library/sdkgo/projectconfig/provider"
)

// forbiddenSDKDriver is the connector-side refresh driver, which a shared entry must never consult.
type forbiddenSDKDriver struct{ t *testing.T }

func (driver forbiddenSDKDriver) RefreshRequired(sdkgo.CredentialRefreshState[token]) bool {
	driver.t.Error("a shared entry must not ask the connector's driver whether to refresh")
	return true
}

func (driver forbiddenSDKDriver) Refresh(context.Context, sdkgo.CredentialRefreshState[token]) (sdkgo.CredentialRefreshResult[token], error) {
	driver.t.Error("a shared entry must not call the connector's driver")
	return sdkgo.CredentialRefreshResult[token]{}, nil
}

var sharedCall = sdkgo.Call{
	Connection: sdkgo.ConnectionRef{Provider: "github", Name: "releases"},
	Operation:  sdkgo.OperationRef{ConnectorID: "github", OperationID: "listReleases"},
}

// The connector's generated NewProjectConnection builds its provider from project.Connections, exactly as these tests do.
func TestGeneratedProviderConstructorsResolveSharedEntriesFromTheBroker(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, sharedAccessOne, epoch, time.Minute))
	ctx := context.Background()

	refreshing, err := provider.NewRefreshingCredentialProvider(fixture.project.Connections, sharedKey, decodeToken, encodeToken)
	require.NoError(t, err)
	value, err := sdkgo.ResolveCredential[token](ctx, refreshing, sharedCall, forbiddenSDKDriver{t})
	require.NoError(t, err)
	require.Equal(t, sharedAccessOne, value.Access)

	fixture.broker.respondAlways(readyReply(2, "second", epoch.Add(time.Minute), time.Hour))
	value, err = sdkgo.ResolveCredentialAfterRejection[token](ctx, refreshing, sharedCall, forbiddenSDKDriver{t})
	require.NoError(t, err)
	require.Equal(t, "second", value.Access)

	fixture.broker.respondAlways(readyReply(2, "second", epoch.Add(time.Minute), time.Hour))
	_, err = sdkgo.ResolveCredentialAfterRejection[token](ctx, refreshing, sharedCall, forbiddenSDKDriver{t})
	require.ErrorIs(t, err, projectconfig.ErrRejectedCredentialNotRefreshed)
	require.False(t, sdkgo.IsReauthorizationRequired(err))

	apiKey, err := provider.NewCredentialProvider(fixture.project.Connections, sharedKey, decodeToken)
	require.NoError(t, err)
	value, err = apiKey.Resolve(sharedCall)
	require.NoError(t, err)
	require.Equal(t, "second", value.Access)
}

func TestSharedReauthorizationRequiredIsTheSDKError(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, sharedAccessOne, epoch, time.Minute))
	fixture.clock.Advance(50 * time.Second)
	fixture.broker.respondAlways(statusReply("REAUTHORIZATION_REQUIRED"))

	refreshing, err := provider.NewRefreshingCredentialProvider(fixture.project.Connections, sharedKey, decodeToken, encodeToken)
	require.NoError(t, err)
	_, err = sdkgo.ResolveCredential[token](context.Background(), refreshing, sharedCall, forbiddenSDKDriver{t})
	require.ErrorIs(t, err, sdkgo.ErrReauthorizationRequired)
	require.True(t, sdkgo.IsReauthorizationRequired(err))
}

func TestProviderGivesTheCallIdentityToTheSharedEntry(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, sharedAccessOne, epoch, time.Hour))
	ctx := context.Background()
	refreshing, err := provider.NewRefreshingCredentialProvider(fixture.project.Connections, sharedKey, decodeToken, encodeToken)
	require.NoError(t, err)
	callA, callB := sharedCall, sharedCall
	callA.ID, callB.ID = "call-a", "call-b"
	for _, call := range []sdkgo.Call{callA, callB} {
		value, err := sdkgo.ResolveCredential[token](ctx, refreshing, call, forbiddenSDKDriver{t})
		require.NoError(t, err)
		require.Equal(t, sharedAccessOne, value.Access)
	}

	fixture.broker.respondAlways(readyReply(2, "second", epoch.Add(5*time.Minute), time.Hour))
	value, err := sdkgo.ResolveCredentialAfterRejection[token](ctx, refreshing, callB, forbiddenSDKDriver{t})
	require.NoError(t, err)
	require.Equal(t, "second", value.Access)
	value, err = sdkgo.ResolveCredentialAfterRejection[token](ctx, refreshing, callA, forbiddenSDKDriver{t})
	require.NoError(t, err)
	require.Equal(t, "second", value.Access)
	require.Equal(t, 2, fixture.broker.requestCount(), "call A's rejection uses the newer revision without a forced read")
}

func TestRejectedCredentialNotRefreshedIsNeverReauthorizationRequired(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, sharedAccessOne, epoch, time.Hour))
	ctx := context.Background()
	refreshing, err := provider.NewRefreshingCredentialProvider(fixture.project.Connections, sharedKey, decodeToken, encodeToken)
	require.NoError(t, err)

	_, err = sdkgo.ResolveCredentialAfterRejection[token](ctx, refreshing, sharedCall, forbiddenSDKDriver{t})
	require.ErrorIs(t, err, projectconfig.ErrRejectedCredentialNotRefreshed)
	require.NotErrorIs(t, err, projectconfig.ErrReauthorizationRequired)
	require.NotErrorIs(t, err, sdkgo.ErrReauthorizationRequired)
	require.False(t, sdkgo.IsReauthorizationRequired(err))

	// The connection stays usable: the credential the provider rejected is still the cached one.
	value, err := sdkgo.ResolveCredential[token](ctx, refreshing, sharedCall, forbiddenSDKDriver{t})
	require.NoError(t, err)
	require.Equal(t, sharedAccessOne, value.Access)

	// A broker that reports REAUTHORIZATION_REQUIRED is the opposite case.
	fixture.clock.Advance(time.Hour)
	fixture.broker.respondAlways(statusReply("REAUTHORIZATION_REQUIRED"))
	_, err = sdkgo.ResolveCredential[token](ctx, refreshing, sharedCall, forbiddenSDKDriver{t})
	require.True(t, sdkgo.IsReauthorizationRequired(err))
}
