// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package projectconfig_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/superdurable/dex-connectors-library/sdkgo/projectconfig"
)

const rotatedToken = "rotated-scope-token"

func TestBrokerRefusalOfTheScopeTokenRenewsItFromTheCurrentConfiguration(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Minute))
	resolver := fixture.resolver(t)
	fixture.rotateScopeToken(t, rotatedToken)
	fixture.broker.respondWith(func(request brokerRequest) brokerReply {
		if request.authorization != "Bearer "+rotatedToken {
			return brokerReply{statusCode: http.StatusUnauthorized, raw: "{}"}
		}
		return readyReply(2, "second", fixture.clock.Now(), time.Minute)
	})

	fixture.clock.Advance(50 * time.Second)
	value, err := resolver.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "second", value.Access)
	require.Equal(t, 3, fixture.broker.requestCount(), "the warm read, the refused read and the retry")
	require.Equal(t, "Bearer "+scopeToken, fixture.broker.request(1).authorization)
	require.Equal(t, "Bearer "+rotatedToken, fixture.broker.request(2).authorization)

	fixture.clock.Advance(50 * time.Second)
	_, err = resolver.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, 4, fixture.broker.requestCount(), "later reads use the renewed token directly")
	require.Equal(t, "Bearer "+rotatedToken, fixture.broker.request(3).authorization)
}

func TestBrokerRefusalOfAnUnrotatedScopeTokenFailsWithoutRetry(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Minute))
	fixture.clock.Advance(50 * time.Second)
	fixture.broker.respondAlways(brokerReply{statusCode: http.StatusUnauthorized, raw: "{}"})

	_, err := fixture.resolver(t).Resolve(context.Background())
	require.ErrorIs(t, err, projectconfig.ErrCredentialBrokerRejected)
	requireNoSecrets(t, err)
	require.Equal(t, 2, fixture.broker.requestCount(), "the same token is not sent again")
}

func TestBrokerRefusalOfARotatedScopeTokenIsRetriedOnce(t *testing.T) {
	fixture := newSharedFixture(t, readyReply(1, "first", epoch, time.Minute))
	fixture.rotateScopeToken(t, rotatedToken)
	fixture.clock.Advance(50 * time.Second)
	fixture.broker.respondAlways(brokerReply{statusCode: http.StatusUnauthorized, raw: "{}"})

	_, err := fixture.resolver(t).Resolve(context.Background())
	require.ErrorIs(t, err, projectconfig.ErrCredentialBrokerRejected)
	require.NotContains(t, err.Error(), rotatedToken)
	requireNoSecrets(t, err)
	require.Equal(t, 3, fixture.broker.requestCount(), "the refused read and exactly one retry")
}
