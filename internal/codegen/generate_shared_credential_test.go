// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package codegen_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/superdurable/dex-connectors-library/internal/codegen"
	"github.com/superdurable/dex-connectors-library/schema"
)

const (
	tokenGrantsModulePath        = "example.com/tokengrants"
	tokenGrantsFixturePath       = "testdata/tokengrants"
	sharedCredentialGoldenPath   = tokenGrantsFixturePath + "/" + codegen.SharedCredentialTestOutputFile
	sharedCredentialManifestPath = "../../schema/testdata/token-grants.yaml"
)

func TestGenerateSharedCredentialTestMatchesGolden(t *testing.T) {
	generated, err := codegen.GenerateSharedCredentialTest(loadTokenGrantsManifest(t), tokenGrantsModulePath)
	require.NoError(t, err)
	if *shouldUpdateGoldenFiles {
		require.NoError(t, os.WriteFile(sharedCredentialGoldenPath, generated, 0o644))
	}
	golden, err := os.ReadFile(sharedCredentialGoldenPath)
	require.NoError(t, err)
	require.Equal(t, string(golden), string(generated), "run go test ./internal/codegen -run MatchesGolden -update-golden")
}

func TestOnlyAConnectorWhoseHostKeepsCredentialsDecodesSharedCredentials(t *testing.T) {
	tokenGrants, err := codegen.Generate(loadTokenGrantsManifest(t))
	require.NoError(t, err)
	require.Contains(t, string(tokenGrants), "func (credentials *Credentials) UnmarshalSharedCredential(contents []byte) error {")
	require.Contains(t, string(tokenGrants), "func (credentials Credentials) validateSharedCredential() error {")
	_, sharedValidation, found := strings.Cut(string(tokenGrants), "validateSharedCredential() error {")
	require.True(t, found)
	_, appOnly, found := strings.Cut(sharedValidation, `case "app-only":`)
	require.True(t, found)
	appOnly, _, _ = strings.Cut(appOnly, "case ")
	require.Contains(t, appOnly, `return fmt.Errorf("credential access_token is required")`, "an application cannot mint the token it needs")
	require.NotContains(t, appOnly, "credential client_secret is required", "the host keeps the client secret")

	apiKeys := string(generateAPIKeyMethodsFixture(t))
	require.NotContains(t, apiKeys, "UnmarshalSharedCredential", "a broker serves an API key unchanged, so the ordinary decoder serves it")
	require.Contains(t, apiKeys, "func decodeCredentials(contents json.RawMessage) (Credentials, error) {")
	_, err = codegen.GenerateSharedCredentialTest(decodeManifestFile(t, apiKeyMethodsManifestPath), "example.com/apikeymethods")
	require.ErrorContains(t, err, "declares no host-kept credentials")
}

// The fixture module runs the generated decode test and the type-checked read-site gate against a hand-written
// client and the local Connector SDK, whose SharedCredentialUnmarshaler the generated method must satisfy.
func TestGeneratedSharedCredentialTestPassesForAConnectorThatKeepsHostKeptFieldsInItsDriver(t *testing.T) {
	output, err := runFixtureModuleTests(t, tokenGrantsModulePath, tokenGrantsFixtureFiles(t, nil))
	require.NoError(t, err, output)
	require.Contains(t, output, "ok  \t"+tokenGrantsModulePath)
}

func TestGeneratedSharedCredentialTestRejectsHostKeptAccessOutsideTheRefreshDriver(t *testing.T) {
	for name, testCase := range map[string]struct {
		path, source, report string
	}{
		"a read in client code": {
			path:   "leak.go",
			source: "package tokengrants\n\nfunc revealRefreshToken(credentials Credentials) string { return credentials.RefreshToken.Reveal() }\n",
			report: "leak.go:3: revealRefreshToken reads RefreshToken",
		},
		"a read in another package of the module": {
			path: "examples/app/main.go",
			source: "package main\n\nimport \"example.com/tokengrants\"\n\nfunc main() {\n\tvar credentials *tokengrants.Credentials\n" +
				"\tprintln(credentials.PrivateKey.Reveal())\n}\n",
			report: "examples/app/main.go:7: main reads PrivateKey",
		},
		"a read in a driver method that neither Refresh nor RefreshRequired uses": {
			path: "required.go",
			source: "package tokengrants\n\nfunc (driver *CredentialRefreshDriver) hasClientSecret(credentials Credentials) bool {\n" +
				"\treturn credentials.ClientSecret.Reveal() != \"\"\n}\n",
			report: "required.go:4: (*CredentialRefreshDriver).hasClientSecret reads ClientSecret",
		},
		"a whole credential encoded": {
			path:   "encode.go",
			source: "package tokengrants\n\nimport \"encoding/json\"\n\nfunc encodeForLog(credentials []Credentials) ([]byte, error) { return json.Marshal(credentials) }\n",
			report: "encode.go:5: encodeForLog passes Credentials to encoding/json.Marshal",
		},
		"a whole credential printed": {
			path:   "print.go",
			source: "package tokengrants\n\nimport \"fmt\"\n\ntype state struct{ credentials Credentials }\n\nfunc describe(value state) string { return fmt.Sprintf(\"%v\", value) }\n",
			report: "print.go:7: describe passes Credentials to fmt.Sprintf",
		},
	} {
		t.Run(name, func(t *testing.T) {
			output, err := runFixtureModuleTests(t, tokenGrantsModulePath, tokenGrantsFixtureFiles(t, map[string]string{testCase.path: testCase.source}))
			require.Error(t, err)
			require.Contains(t, output, "--- FAIL: TestHostKeptCredentialsStayInTheRefreshDriver")
			require.Contains(t, output, testCase.report)
		})
	}
}

func TestGeneratedSharedCredentialTestAllowsAHostKeptReadInRefreshRequired(t *testing.T) {
	client, err := os.ReadFile(filepath.Join(tokenGrantsFixturePath, "client.go"))
	require.NoError(t, err)
	const accessTokenCheck = `return state.Credentials.AccessToken.Reveal() == ""`
	require.Contains(t, string(client), accessTokenCheck)
	edited := strings.Replace(string(client), accessTokenCheck, `return state.Credentials.RefreshToken.Reveal() != ""`, 1)

	output, err := runFixtureModuleTests(t, tokenGrantsModulePath, tokenGrantsFixtureFiles(t, map[string]string{"client.go": edited}))

	require.NoError(t, err, "a shared entry never calls RefreshRequired, so it may decide with a refresh token: %s", output)
}

func tokenGrantsFixtureFiles(t *testing.T, extra map[string]string) map[string][]byte {
	t.Helper()
	manifest := loadTokenGrantsManifest(t)
	connector, err := codegen.Generate(manifest)
	require.NoError(t, err)
	sharedCredentialTest, err := codegen.GenerateSharedCredentialTest(manifest, tokenGrantsModulePath)
	require.NoError(t, err)
	files := map[string][]byte{codegen.OutputFile: connector, codegen.SharedCredentialTestOutputFile: sharedCredentialTest}
	for _, name := range []string{"client.go", "shared_credential_unmarshaler_test.go"} {
		contents, readErr := os.ReadFile(filepath.Join(tokenGrantsFixturePath, name))
		require.NoError(t, readErr)
		files[name] = contents
	}
	for path, source := range extra {
		files[path] = []byte(source)
	}
	return files
}

func loadTokenGrantsManifest(t *testing.T) schema.Manifest {
	t.Helper()
	return decodeManifestFile(t, sharedCredentialManifestPath)
}

func decodeManifestFile(t *testing.T, path string) schema.Manifest {
	t.Helper()
	file, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	manifest, err := schema.Decode(file)
	require.NoError(t, err)
	return manifest
}
