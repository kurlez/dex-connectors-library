// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package projectconfig_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/superdurable/dex-connectors-library/sdkgo/projectconfig"
)

type localCredentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

type localSettings struct {
	Workspace string `json:"workspace"`
}

type localDriver struct {
	refreshes int
}

func (*localDriver) RefreshRequired(projectconfig.RefreshState[localCredentials]) bool { return false }

func (driver *localDriver) Refresh(_ context.Context, state projectconfig.RefreshState[localCredentials]) (projectconfig.RefreshResult[localCredentials], error) {
	driver.refreshes++
	return projectconfig.RefreshResult[localCredentials]{
		Credentials: localCredentials{AccessToken: "access-2", RefreshToken: "refresh-2"},
		ExpiresAt:   state.Now.Add(time.Hour),
	}, nil
}

var localKey = projectconfig.ConnectionKey{ConnectorID: "example", ConnectionName: "main"}

func decodeLocalCredentials(contents json.RawMessage) (localCredentials, error) {
	var credentials localCredentials
	return credentials, projectconfig.DecodeCredentials(contents, &credentials)
}

func encodeLocalCredentials(credentials localCredentials) (json.RawMessage, error) {
	return json.Marshal(credentials)
}

// writeDexWebFiles writes the files of Dex Web at cli-v1.7.0 (superdurable/dex 37cf179ed): a connections file with
// the members of its localConnectorConnection, and the use configurations sidecar.
func writeDexWebFiles(t *testing.T, directory, accessToken string, expiresAt *time.Time) string {
	t.Helper()
	connection := map[string]any{
		"connectorId": "example", "authMethodId": "oauth", "authMethodIds": []string{"oauth", "api_key"},
		"modulePath": "github.com/superdurable/dex-connectors-library/connectors/example", "moduleVersion": "v0.21.0",
		"provider": "example", "connectionName": "main", "futureMember": "kept",
		"configuration": map[string]any{"workspace": "acme"},
		"credentials":   map[string]any{"access_token": accessToken, "refresh_token": "refresh-1"},
	}
	if expiresAt != nil {
		connection["credentialExpiresAt"] = expiresAt.UTC().Format(time.RFC3339Nano)
	}
	connections := map[string]any{
		"schemaVersion": "connectors.dex.dev/local-connections/v1alpha1",
		"connections":   []any{connection},
		"triggerBindings": []any{map[string]any{
			"connectorId": "example", "connectionName": "main", "triggerName": "messageCreated", "bindingName": "inbound",
			"configuration": map[string]any{"channel": "C1"},
		}},
	}
	path := filepath.Join(directory, "connections.json")
	writeJSON(t, path, connections)
	writeJSON(t, filepath.Join(directory, "use-configurations.json"), map[string]any{
		"schemaVersion": "connectors.dex.dev/local-use-configurations/v1alpha1",
		"operationConfigurations": []any{map[string]any{
			"connectorId": "example", "connectionName": "main", "operationId": "send", "flowType": "Notify", "stepType": "Send",
			"configuration": map[string]any{"model": "fast"},
		}},
	})
	return path
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	contents, err := json.MarshalIndent(value, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	require.NoError(t, os.Chmod(path, 0o600))
}

func loadLocal(t *testing.T, path string) *projectconfig.LoadedProject {
	t.Helper()
	t.Setenv(projectconfig.LocalConnectionsFileEnvironment, path)
	project, err := projectconfig.LoadFromEnvironment(context.Background())
	require.NoError(t, err)
	return project
}

func localResolver(t *testing.T, project *projectconfig.LoadedProject) *projectconfig.CredentialResolver[localCredentials] {
	t.Helper()
	resolver, err := projectconfig.NewCredentialResolver(project.Connections, localKey, decodeLocalCredentials, encodeLocalCredentials)
	require.NoError(t, err)
	return resolver
}

func TestLocalConnectionsFileResolvesSettingsAndCredentials(t *testing.T) {
	directory := t.TempDir()
	future := time.Now().Add(time.Hour)
	project := loadLocal(t, writeDexWebFiles(t, directory, "access-1", &future))

	var settings localSettings
	require.NoError(t, project.Configuration.DecodeConnectionConfiguration(localKey, &settings))
	require.Equal(t, "acme", settings.Workspace)
	var operation map[string]string
	require.NoError(t, project.Configuration.DecodeOperationConfiguration("example", "main", "send", "Notify", "Send", &operation))
	require.Equal(t, "fast", operation["model"])
	var trigger map[string]string
	require.NoError(t, project.Configuration.DecodeTriggerConfiguration("example", "main", "messageCreated", "inbound", &trigger))
	require.Equal(t, "C1", trigger["channel"])

	credentials, err := localResolver(t, project).Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, localCredentials{AccessToken: "access-1", RefreshToken: "refresh-1"}, credentials)
}

func TestLocalConnectionsFileAdoptsReauthorizationWithoutRestart(t *testing.T) {
	directory := t.TempDir()
	future := time.Now().Add(time.Hour)
	path := writeDexWebFiles(t, directory, "access-1", &future)
	resolver := localResolver(t, loadLocal(t, path))
	_, err := resolver.Resolve(context.Background())
	require.NoError(t, err)

	writeDexWebFiles(t, directory, "access-reauthorized", &future)
	credentials, err := resolver.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "access-reauthorized", credentials.AccessToken)
}

func TestLocalConnectionsFileWritesRefreshedCredentialBack(t *testing.T) {
	directory := t.TempDir()
	expired := time.Now().Add(-time.Minute)
	path := writeDexWebFiles(t, directory, "access-1", &expired)
	driver := &localDriver{}
	credentials, err := localResolver(t, loadLocal(t, path)).ResolveWithRefresh(context.Background(), driver)
	require.NoError(t, err)
	require.Equal(t, "access-2", credentials.AccessToken)
	require.Equal(t, 1, driver.refreshes)

	var file struct {
		Connections []map[string]json.RawMessage `json:"connections"`
	}
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(contents, &file))
	require.JSONEq(t, `{"access_token":"access-2","refresh_token":"refresh-2"}`, string(file.Connections[0]["credentials"]))
	require.Contains(t, file.Connections[0], "credentialExpiresAt")
	require.JSONEq(t, `"kept"`, string(file.Connections[0]["futureMember"]))
	require.JSONEq(t, `["oauth","api_key"]`, string(file.Connections[0]["authMethodIds"]))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// A restarted application starts from the refreshed credential.
	restarted, err := localResolver(t, loadLocal(t, path)).ResolveWithRefresh(context.Background(), driver)
	require.NoError(t, err)
	require.Equal(t, "refresh-2", restarted.RefreshToken)
	require.Equal(t, 1, driver.refreshes)
}

func TestLocalConnectionsFileKeepsNewerCredentialFromDexWeb(t *testing.T) {
	directory := t.TempDir()
	expired := time.Now().Add(-time.Minute)
	path := writeDexWebFiles(t, directory, "access-1", &expired)
	resolver := localResolver(t, loadLocal(t, path))
	// Dex Web reauthorizes after the application loaded, so the next call adopts it and needs no refresh.
	future := time.Now().Add(time.Hour)
	writeDexWebFiles(t, directory, "access-reauthorized", &future)
	driver := &localDriver{}
	credentials, err := resolver.ResolveWithRefresh(context.Background(), driver)
	require.NoError(t, err)
	require.Equal(t, "access-reauthorized", credentials.AccessToken)
	require.Zero(t, driver.refreshes)
}

func TestLocalTriggerInboxSurvivesRestart(t *testing.T) {
	directory := t.TempDir()
	future := time.Now().Add(time.Hour)
	path := writeDexWebFiles(t, directory, "access-1", &future)
	key := projectconfig.TriggerInboxKey{ConnectorID: "example", ConnectionName: "main", TriggerName: "messageCreated", BindingName: "inbound"}
	inbox, err := loadLocal(t, path).TriggerInbox(key)
	require.NoError(t, err)
	require.NoError(t, inbox.Add(context.Background(), projectconfig.PendingTriggerEvent{ID: "event-1", Event: json.RawMessage(`{"text":"hi"}`)}))

	restarted, err := loadLocal(t, path).TriggerInbox(key)
	require.NoError(t, err)
	pending, err := restarted.Pending(context.Background())
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "event-1", pending[0].ID)
}

func TestLocalConnectionsFileRejectsUnsafeOrAmbiguousSetup(t *testing.T) {
	future := time.Now().Add(time.Hour)
	load := func(path string) error {
		t.Setenv(projectconfig.LocalConnectionsFileEnvironment, path)
		_, err := projectconfig.LoadFromEnvironment(context.Background())
		return err
	}

	path := writeDexWebFiles(t, t.TempDir(), "access-1", &future)
	t.Setenv("DEX_PROJECT_ID", "project-1")
	require.ErrorContains(t, load(path), "select different project configurations")
	t.Setenv("DEX_PROJECT_ID", "")

	require.NoError(t, os.Chmod(path, 0o644))
	require.ErrorContains(t, load(path), "permissions must be 0600")

	wrongSchema := filepath.Join(t.TempDir(), "connections.json")
	writeJSON(t, wrongSchema, map[string]any{"schemaVersion": "other", "connections": []any{}})
	require.ErrorContains(t, load(wrongSchema), "unsupported connector configuration schema version")

	link := filepath.Join(t.TempDir(), "connections.json")
	require.NoError(t, os.Symlink(path, link))
	require.ErrorContains(t, load(link), "regular file")

	require.Error(t, load(filepath.Join(t.TempDir(), "missing.json")))
}
