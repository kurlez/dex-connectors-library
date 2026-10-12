// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package schema_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/superdurable/dex-connectors-library/schema"
)

func TestHostKeptCredentialsFollowEveryGrantDeclaration(t *testing.T) {
	hostKept := map[string][]string{}
	for _, method := range decodeFixture(t, "token-grants.yaml").Spec.Auth.AuthMethods() {
		hostKept[method.ID] = schema.HostKeptCredentials(method)
	}
	require.Equal(t, map[string][]string{
		"user-oauth": {"client_secret", "refresh_token"},
		"bot-oauth":  {"client_secret", "bot_refresh_token", "user_refresh_token"},
		"app-only":   {"client_secret"},
		"delegation": {"service_account_key"},
		"jwt-bearer": {"private_key"},
	}, hostKept, "the client ID, the access tokens and every other field reach applications")
}

func TestHostKeptCredentialsIncludeNestedRefreshTokensWithoutGrants(t *testing.T) {
	manifest := decodeEditedFixture(t, "token-grants.yaml", manifestEdit{
		"          refresh:\n            missingExpiry: keep\n            grants:\n              - refreshTokenCredential: bot_refresh_token\n                credentialMappings:\n                  - {credential: bot_token, source: access_token}\n                  - {credential: bot_refresh_token, source: refresh_token}\n              - refreshTokenCredential: user_refresh_token\n                grantedScopes: userScopes\n                credentialMappings:\n                  - {credential: user_token, source: access_token}\n                  - {credential: user_refresh_token, source: refresh_token}\n",
		"",
	})
	for _, method := range manifest.Spec.Auth.Methods {
		if method.ID == "bot-oauth" {
			require.Equal(t, []string{"client_secret", "bot_refresh_token", "user_refresh_token"}, schema.HostKeptCredentials(method))
		}
	}
}

func TestStaticMethodsKeepNothingOnTheHost(t *testing.T) {
	methods := decodeFixture(t, "api-key-methods.yaml").Spec.Auth.AuthMethods()
	require.NotEmpty(t, methods)
	for _, method := range methods {
		require.Empty(t, schema.HostKeptCredentials(method), method.ID)
	}
}

func TestSingleMethodAuthIsOneMethodWithTheEmptyID(t *testing.T) {
	methods := decodeFixture(t, "google-oauth.yaml").Spec.Auth.AuthMethods()
	require.Len(t, methods, 1)
	require.Empty(t, methods[0].ID)
	require.Empty(t, schema.HostKeptCredentials(methods[0]), "a method without a client secret or refresh token keeps nothing")
	withSecret := decodeEditedFixture(t, "google-oauth.yaml",
		manifestEdit{"        required: true\n    guide:", "        required: true\n      - {name: client_id, goName: ClientID, type: string, description: Client ID., required: true}\n      - {name: client_secret, goName: ClientSecret, type: secretString, description: Client secret., required: true}\n    guide:"},
		manifestEdit{"      pkce: true", "      pkce: true\n      clientIDCredential: client_id\n      clientSecretCredential: client_secret"},
	)
	require.Equal(t, []string{"client_secret"}, schema.HostKeptCredentials(withSecret.Spec.Auth.AuthMethods()[0]))
}
