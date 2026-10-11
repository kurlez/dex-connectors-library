// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package codegen_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/superdurable/dex-connectors-library/internal/codegen"
	"github.com/superdurable/dex-connectors-library/schema"
)

const refreshAgreementGoldenPath = "testdata/tokengrants/" + codegen.RefreshAgreementTestOutputFile

func TestGenerateRefreshAgreementTestMatchesGolden(t *testing.T) {
	generated := generateTokenGrantsAgreement(t, nil)
	if *shouldUpdateGoldenFiles {
		require.NoError(t, os.MkdirAll(filepath.Dir(refreshAgreementGoldenPath), 0o755))
		require.NoError(t, os.WriteFile(refreshAgreementGoldenPath, generated, 0o644))
	}
	golden, err := os.ReadFile(refreshAgreementGoldenPath)
	require.NoError(t, err)
	require.Equal(t, string(golden), string(generated), "run go test ./internal/codegen -run MatchesGolden -update-golden")
}

func TestGenerateRefreshAgreementTestDeclaresEveryGrant(t *testing.T) {
	text := string(generateTokenGrantsAgreement(t, nil))
	for _, expected := range []string{
		`name:     "user-oauth refresh",`,
		`clientAuth:         "client_secret_basic",`,
		`encoding:           "json",`,
		`"refresh_token": "manifest-refresh-token", "scope": "offline_access files.read"}`,
		`name:     "bot-oauth refresh user_refresh_token",`,
		`"scope": "history.read"`,
		`name:     "app-only clientCredentials",`,
		`endpoint:           "https://login.example.com/manifest.example.com/token",`,
		`name:     "delegation jwtBearer",`,
		`ServiceAccountKey: sdkgo.NewSecretString(manifestJSONKey(privateKeyPEM, "private_key", map[string]string{"client_email": "manifest-issuer@example.com", "token_uri": "https://manifest.example.com/token"})),`,
		`"primary_email": "manifest-user@example.com"`,
		`name:     "jwt-bearer jwtBearer with login_environment \"sandbox\"",`,
		`audience: "https://test.example.com"`,
		`missingExpiry:      time.Duration(7200000000000),`,
	} {
		require.Contains(t, text, expected)
	}
	require.NotContains(t, text, `"scope": "offline_access files.read", "token_type"`, "a response does not report offline_access")
}

func TestGenerateRefreshAgreementTestAppliesSamplesFromGeneralToSpecific(t *testing.T) {
	text := string(generateTokenGrantsAgreement(t, map[string]codegen.RefreshAgreementSample{
		"*":                                    {Response: map[string]string{"instance_url": "https://general.example.com"}},
		"user-oauth":                           {Response: map[string]string{"instance_url": "https://method.example.com"}},
		"user-oauth refresh":                   {Credentials: map[string]string{"instance_url": "https://stored.example.com"}},
		"bot-oauth refresh user_refresh_token": {Response: map[string]string{"token_type": "user"}},
	}))
	require.Contains(t, text, `"instance_url": "https://method.example.com"`, "a method sample overrides the general one")
	require.Contains(t, text, `"instance_url": "https://general.example.com"`, "the general sample applies to the other methods")
	require.Contains(t, text, `"https://stored.example.com"`)
	require.Contains(t, text, `"token_type": "user"`)
}

func generateTokenGrantsAgreement(t *testing.T, samples map[string]codegen.RefreshAgreementSample) []byte {
	t.Helper()
	file, err := os.Open("../../schema/testdata/token-grants.yaml")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	manifest, err := schema.Decode(file)
	require.NoError(t, err)
	require.True(t, codegen.HasDeclaredTokenGrants(manifest))
	generated, err := codegen.GenerateRefreshAgreementTest(manifest, "example.com/tokengrants", samples)
	require.NoError(t, err)
	return generated
}
