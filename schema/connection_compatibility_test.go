// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package schema_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/superdurable/dex-connectors-library/schema"
)

// manifestEdit replaces one exact fragment of a fixture manifest.
type manifestEdit struct{ old, replacement string }

func TestConnectionCompatibilityAcceptsAdditiveChanges(t *testing.T) {
	for name, testCase := range map[string]struct {
		fixture string
		edits   []manifestEdit
	}{
		"descriptions, Go names, defaults and endpoints": {fixture: "google-oauth.yaml", edits: []manifestEdit{
			{"description: OAuth application display name.", "description: The OAuth application name."},
			{"goName: ApplicationName", "goName: ApplicationDisplayName"},
			{"default: production", "default: sandbox"},
			{"tokenEndpoint: https://oauth2.googleapis.com/token", "tokenEndpoint: https://oauth2.googleapis.com/v2/token"},
		}},
		"a required setting becomes optional": {fixture: "google-oauth.yaml", edits: []manifestEdit{
			{"description: OAuth application display name.\n        required: true", "description: OAuth application display name.\n        required: false"},
		}},
		"a setting gains an enum value": {fixture: "google-oauth.yaml", edits: []manifestEdit{
			{"enum: [production, sandbox]", "enum: [production, sandbox, staging]"},
		}},
		"an optional setting is added": {fixture: "google-oauth.yaml", edits: []manifestEdit{
			{"  auth:\n", "      - {name: region, goName: Region, type: string, description: Region., required: false}\n  auth:\n"},
		}},
		"user scopes stay empty": {fixture: "multi-auth.yaml", edits: []manifestEdit{
			{"scopes: [openid]", "scopes: [openid]\n          userScopes: []"},
		}},
		"a method is added": {fixture: "api-key-methods.yaml", edits: []manifestEdit{
			{"      - id: gemini\n", "      - id: mistral\n        displayName: Mistral\n        description: Use a Mistral API key.\n        type: apiKey\n        connectionKind: example-mistral-api-key\n        fields:\n          - {name: mistral_api_key, goName: MistralAPIKey, type: secretString, description: Secret Mistral API key., required: true}\n        guide:\n          startURL: https://console.mistral.ai/api-keys\n          steps: [Create a key.]\n      - id: gemini\n"},
		}},
		"a method configuration gains an optional field": {fixture: "api-key-methods.yaml", edits: []manifestEdit{
			{"            - {name: openaiProjectId, ", "            - {name: openaiOrganizationId, goName: OpenAIOrganizationID, type: string, description: Organization ID., required: false}\n            - {name: openaiProjectId, "},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			released := decodeFixture(t, testCase.fixture)
			candidate := decodeEditedFixture(t, testCase.fixture, testCase.edits...)
			require.NoError(t, schema.ValidateConnectionCompatibility(released, candidate))
		})
	}
}

func TestConnectionCompatibilityRejectsContractChanges(t *testing.T) {
	for name, testCase := range map[string]struct {
		fixture string
		edits   []manifestEdit
		breaks  []string
	}{
		"a setting is removed and another renamed": {fixture: "google-oauth.yaml", edits: []manifestEdit{
			{"      - name: labels\n", "      - name: tags\n"},
			{"      - name: applicationName\n        goName: ApplicationName\n        type: string\n        description: OAuth application display name.\n        required: true\n", ""},
		}, breaks: []string{"spec.configuration field applicationName is removed", "spec.configuration field labels is removed"}},
		"a setting is retyped, made required and loses an enum value": {fixture: "google-oauth.yaml", edits: []manifestEdit{
			{"type: stringList", "type: string"},
			{"description: Optional scope override fixture.\n        required: false\n        default: [scope-b, scope-a]", "description: Optional scope override fixture.\n        required: true"},
			{"enum: [production, sandbox]", "enum: [production]"},
		}, breaks: []string{
			"spec.configuration field scopesOverride changes type from stringList to string",
			"spec.configuration field scopesOverride becomes required",
			"spec.configuration field environment removes enum values sandbox",
		}},
		"a required setting is added": {fixture: "google-oauth.yaml", edits: []manifestEdit{
			{"  auth:\n", "      - {name: region, goName: Region, type: string, description: Region., required: true}\n  auth:\n"},
		}, breaks: []string{"spec.configuration adds required field region"}},
		"a credential field changes": {fixture: "multi-auth.yaml", edits: []manifestEdit{
			{"{name: access_token, goName: AccessToken, type: secretString, description: Short-lived access token., required: true}", "{name: access_token, goName: AccessToken, type: secretString, description: Short-lived access token., required: false}"},
			{"{name: delegated_user,", "{name: delegated_principal,"},
			{"          - {name: service_account_key, ", "          - {name: key_id, goName: KeyID, type: string, description: Key ID., required: false}\n          - {name: service_account_key, "},
		}, breaks: []string{
			"auth method google-oauth credential field access_token changes required from true to false",
			"auth method workspace-service-account credential field delegated_user is removed",
			"auth method workspace-service-account adds credential field key_id",
			"auth method workspace-service-account adds credential field delegated_principal",
		}},
		"scopes change": {fixture: "multi-auth.yaml", edits: []manifestEdit{
			{"scopes: [openid]", "scopes: [email]\n          userScopes: [profile]"},
		}, breaks: []string{"auth method google-oauth OAuth scopes add email", "auth method google-oauth OAuth scopes remove openid", "auth method google-oauth OAuth user scopes add profile"}},
		"a method is renamed and another retyped": {fixture: "multi-auth.yaml", edits: []manifestEdit{
			{"type: serviceAccount", "type: apiKey"},
			{"defaultMethod: google-oauth", "defaultMethod: google-user-oauth"},
			{"      - id: google-oauth\n", "      - id: google-user-oauth\n"},
		}, breaks: []string{
			"spec.auth.defaultMethod changes from google-oauth to google-user-oauth",
			"auth method google-oauth is removed",
			"auth method workspace-service-account changes type from serviceAccount to apiKey",
		}},
		"the default method changes": {fixture: "multi-auth.yaml", edits: []manifestEdit{
			{"defaultMethod: google-oauth", "defaultMethod: workspace-service-account"},
		}, breaks: []string{"spec.auth.defaultMethod changes from google-oauth to workspace-service-account, which changes the method of credentials saved without one"}},
		"a method configuration field is made required and another loses an enum value": {fixture: "api-key-methods.yaml", edits: []manifestEdit{
			{"description: Claude workspace ID for a multi-workspace key., required: false}", "description: Claude workspace ID for a multi-workspace key., required: true}"},
			{"enum: [v1, v1beta], default: v1beta", "enum: [v1beta]"},
		}, breaks: []string{
			"auth method anthropic configuration field anthropicWorkspaceId becomes required",
			"auth method gemini configuration field geminiApiVersion removes enum values v1",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			released := decodeFixture(t, testCase.fixture)
			candidate := decodeEditedFixture(t, testCase.fixture, testCase.edits...)
			err := schema.ValidateConnectionCompatibility(released, candidate)
			require.Error(t, err)
			for _, contractBreak := range testCase.breaks {
				require.ErrorContains(t, err, contractBreak)
			}
		})
	}
}

func TestConnectionCompatibilityComparesScopesFromASettingByItsName(t *testing.T) {
	released := decodeFixture(t, "oauth-from-settings.yaml")
	require.NoError(t, schema.ValidateConnectionCompatibility(released, decodeFixture(t, "oauth-from-settings.yaml")))

	renamed := decodeEditedFixture(t, "oauth-from-settings.yaml",
		manifestEdit{"      - {name: scopes, goName: Scopes, type: stringList, description: Scopes to request., required: true}", "      - {name: scopes, goName: Scopes, type: stringList, description: Scopes to request., required: true}\n      - {name: requestedScopes, goName: RequestedScopes, type: stringList, description: Scopes to request., required: true}"},
		manifestEdit{"scopes: {fromSetting: scopes}", "scopes: {fromSetting: requestedScopes}"})
	require.ErrorContains(t, schema.ValidateConnectionCompatibility(released, renamed), "spec.auth OAuth scopes change from setting scopes to setting requestedScopes")

	fixed := decodeEditedFixture(t, "oauth-from-settings.yaml", manifestEdit{"scopes: {fromSetting: scopes}", "scopes: [openid]"})
	require.ErrorContains(t, schema.ValidateConnectionCompatibility(released, fixed), "spec.auth OAuth scopes change from setting scopes to a fixed list")
	require.ErrorContains(t, schema.ValidateConnectionCompatibility(fixed, released), "spec.auth OAuth scopes change from a fixed list to setting scopes")
}

func TestConnectionCompatibilityResolvesASingleMethodThroughTheDefaultMethod(t *testing.T) {
	released := decodeFixture(t, "google-oauth.yaml")
	const methods = `  auth:
    defaultMethod: %s
    methods:
      - id: google-oauth
        displayName: Google OAuth
        description: Authorize a Google account.
        type: oauth2
        connectionKind: google-sheets-oauth
        fields:
          - {name: access_token, goName: AccessToken, type: secretString, description: OAuth access token., required: true}
        guide: {startURL: https://console.cloud.google.com/apis/credentials, steps: [Create an OAuth client.]}
        oauth2:
          authorizationEndpoint: https://accounts.google.com/o/oauth2/v2/auth
          tokenEndpoint: https://oauth2.googleapis.com/token
          scopes: [https://www.googleapis.com/auth/drive.file]
          pkce: true
      - id: api-key
        displayName: API key
        description: Use an API key.
        type: apiKey
        connectionKind: google-sheets-api-key
        fields:
          - {name: api_key, goName: APIKey, type: secretString, description: API key., required: true}
        guide: {startURL: https://console.cloud.google.com/apis/credentials, steps: [Create an API key.]}
  operations:
`
	singleMethod := readFixture(t, "google-oauth.yaml")
	auth := singleMethod[strings.Index(singleMethod, "  auth:\n") : strings.Index(singleMethod, "  operations:\n")+len("  operations:\n")]
	for defaultMethod, contractBreak := range map[string]string{
		"google-oauth": "",
		"api-key":      "spec.auth, now auth method api-key, changes type from oauth2 to apiKey",
	} {
		candidate := decodeManifest(t, strings.Replace(singleMethod, auth, strings.Replace(methods, "%s", defaultMethod, 1), 1))
		err := schema.ValidateConnectionCompatibility(released, candidate)
		if contractBreak == "" {
			require.NoError(t, err, defaultMethod)
			continue
		}
		require.ErrorContains(t, err, contractBreak, defaultMethod)
	}
	require.ErrorContains(t, schema.ValidateConnectionCompatibility(decodeFixture(t, "multi-auth.yaml"), released),
		"auth method google-oauth is removed", "a single method cannot keep the IDs of several methods")
}

func TestDecodeReleasedManifestReadsFieldsThatValidationNowRejects(t *testing.T) {
	contents := strings.Replace(readFixture(t, "google-oauth.yaml"), "kind: Connector\n", "kind: Connector\nretiredField: true\n", 1)
	contents = strings.Replace(contents, "version: v0.1.0", "version: 0.1", 1)
	_, err := schema.Decode(strings.NewReader(contents))
	require.Error(t, err)
	released, err := schema.DecodeReleasedManifest(strings.NewReader(contents))
	require.NoError(t, err)
	require.Equal(t, "applicationName", released.Spec.Configuration.Fields[0].Name)
	require.NoError(t, schema.ValidateConnectionCompatibility(released, decodeFixture(t, "google-oauth.yaml")))
}

func decodeFixture(t *testing.T, fixture string) schema.Manifest {
	t.Helper()
	return decodeManifest(t, readFixture(t, fixture))
}

func decodeEditedFixture(t *testing.T, fixture string, edits ...manifestEdit) schema.Manifest {
	t.Helper()
	contents := readFixture(t, fixture)
	for _, edit := range edits {
		require.Contains(t, contents, edit.old)
		contents = strings.Replace(contents, edit.old, edit.replacement, 1)
	}
	return decodeManifest(t, contents)
}

func decodeManifest(t *testing.T, contents string) schema.Manifest {
	t.Helper()
	manifest, err := schema.Decode(strings.NewReader(contents))
	require.NoError(t, err)
	return manifest
}

func readFixture(t *testing.T, fixture string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("testdata", fixture))
	require.NoError(t, err)
	return string(contents)
}
