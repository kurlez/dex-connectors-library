// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package projectconfig_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/superdurable/dex-connectors-library/sdkgo/internal/testsupport"
	"github.com/superdurable/dex-connectors-library/sdkgo/projectconfig"
)

const (
	currentSchemaVersion  = "connectors.dex.dev/project-configuration/v1alpha3"
	previousSchemaVersion = "connectors.dex.dev/project-configuration/v1alpha2"
	brokerTokenKeyPrefix  = "projects/shared/live/app-secrets/DEX_CREDENTIAL_BROKER_TOKEN/"
)

var (
	sharedScope  = projectconfig.Scope{ProjectID: "shared", Kind: "live"}
	previewScope = projectconfig.Scope{ProjectID: "shared", Kind: "preview"}
)

func newConfigurationStore(t *testing.T) (*projectconfig.ConfigurationStore, *testsupport.ObjectStore) {
	t.Helper()
	objects := testsupport.NewObjectStore()
	store, err := projectconfig.NewConfigurationStore(&projectconfig.ConfigurationStoreConfig{Objects: objects, Scope: sharedScope})
	require.NoError(t, err)
	return store, objects
}

func credentialBroker(t *testing.T, store *projectconfig.ConfigurationStore, endpoint string) *projectconfig.CredentialBrokerConfiguration {
	t.Helper()
	reference, err := store.CreateCredentialBrokerToken(context.Background(), "scope-token")
	require.NoError(t, err)
	return &projectconfig.CredentialBrokerConfiguration{Endpoint: endpoint, TokenSecretRef: reference}
}

func ordinaryEntry(name string) projectconfig.ConnectionConfiguration {
	return projectconfig.ConnectionConfiguration{ConnectorID: "github", ConnectionName: name, ModulePath: "example.test/github", Provider: "github", Configuration: json.RawMessage(`{"maxResponseBytes":4096}`)}
}

func sharedEntry(name, sharedConnectionID string) projectconfig.ConnectionConfiguration {
	return projectconfig.ConnectionConfiguration{
		ConnectorID: "github", ConnectionName: name, ModulePath: "example.test/github", Provider: "github", SharedConnectionID: sharedConnectionID,
		Configuration: json.RawMessage(`{"maxResponseBytes":8192}`),
	}
}

func TestSharedConnectionsRoundTripAtTheCurrentSchemaVersion(t *testing.T) {
	store, _ := newConfigurationStore(t)
	broker := credentialBroker(t, store, "https://broker.example.test/internal/connection-credentials/read")
	require.Contains(t, broker.TokenSecretRef.Key, brokerTokenKeyPrefix)

	written, _, err := store.WriteConfiguration(context.Background(), 0, projectconfig.Configuration{
		Connections:      []projectconfig.ConnectionConfiguration{ordinaryEntry("local"), sharedEntry("releases", "conn-7")},
		CredentialBroker: broker,
	})
	require.NoError(t, err)
	require.Equal(t, currentSchemaVersion, written.SchemaVersion)
	require.Equal(t, projectconfig.ConfigurationSchemaVersion, written.SchemaVersion)

	read, _, err := store.ReadConfiguration(context.Background())
	require.NoError(t, err)
	require.Equal(t, broker, read.CredentialBroker)
	require.Equal(t, "conn-7", read.Connections[1].SharedConnectionID)
	require.Empty(t, read.Connections[0].SharedConnectionID)
	var ordinary, shared githubSettings
	require.NoError(t, read.DecodeConnectionConfiguration(ordinaryKey, &ordinary))
	require.Equal(t, 4096, ordinary.MaxResponseBytes)
	require.NoError(t, read.DecodeConnectionConfiguration(sharedKey, &shared))
	require.Equal(t, 8192, shared.MaxResponseBytes, "a shared entry keeps its settings in the document")
}

func TestDocumentWithoutSharedConnectionsKeepsThePreviousSchemaVersion(t *testing.T) {
	store, _ := newConfigurationStore(t)
	written, _, err := store.WriteConfiguration(context.Background(), 0, projectconfig.Configuration{Connections: []projectconfig.ConnectionConfiguration{ordinaryEntry("local")}})
	require.NoError(t, err)
	require.Equal(t, previousSchemaVersion, written.SchemaVersion)
	read, _, err := store.ReadConfiguration(context.Background())
	require.NoError(t, err)
	require.Equal(t, previousSchemaVersion, read.SchemaVersion)

	// A broker without any shared entry still needs the current version.
	written, _, err = store.WriteConfiguration(context.Background(), 1, projectconfig.Configuration{CredentialBroker: credentialBroker(t, store, "https://broker.example.test/read")})
	require.NoError(t, err)
	require.Equal(t, currentSchemaVersion, written.SchemaVersion)
}

func TestPreviousSchemaVersionDocumentLoadsAndRejectsSharedConnections(t *testing.T) {
	document := func(connection string) []byte {
		return []byte(`{"schemaVersion":"` + previousSchemaVersion + `","scope":{"projectId":"shared","kind":"live"},"revision":1,` +
			`"connections":[` + connection + `],"triggerBindings":[],"operationConfigurations":[]}`)
	}
	ordinary := `{"connectorId":"github","connectionName":"local","modulePath":"example.test/github","provider":"github","configuration":{"maxResponseBytes":4096}}`
	shared := `{"connectorId":"github","connectionName":"local","modulePath":"example.test/github","provider":"github","sharedConnectionId":"conn-7","configuration":{}}`

	for name, test := range map[string]struct {
		connection string
		isValid    bool
	}{
		"ordinary entry":                    {connection: ordinary, isValid: true},
		"shared entry under the old schema": {connection: shared},
	} {
		t.Run(name, func(t *testing.T) {
			store, objects := newConfigurationStore(t)
			_, err := objects.CreateObject(context.Background(), "projects/shared/live/configuration/head", document(test.connection))
			require.NoError(t, err)
			read, _, err := store.ReadConfiguration(context.Background())
			if !test.isValid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, previousSchemaVersion, read.SchemaVersion)
			require.Equal(t, "local", read.Connections[0].ConnectionName)
		})
	}

	t.Run("broker under the old schema", func(t *testing.T) {
		store, objects := newConfigurationStore(t)
		broker := credentialBroker(t, store, "https://broker.example.test/read")
		brokerJSON, err := json.Marshal(broker)
		require.NoError(t, err)
		contents := []byte(`{"schemaVersion":"` + previousSchemaVersion + `","scope":{"projectId":"shared","kind":"live"},"revision":1,"connections":[],` +
			`"triggerBindings":[],"operationConfigurations":[],"credentialBroker":` + string(brokerJSON) + `}`)
		_, err = objects.CreateObject(context.Background(), "projects/shared/live/configuration/head", contents)
		require.NoError(t, err)
		_, _, err = store.ReadConfiguration(context.Background())
		require.Error(t, err)
	})
}

func TestUnsupportedSchemaVersionIsRejected(t *testing.T) {
	store, objects := newConfigurationStore(t)
	contents := []byte(`{"schemaVersion":"connectors.dex.dev/project-configuration/v1alpha4","scope":{"projectId":"shared","kind":"live"},"revision":1,"connections":[],"triggerBindings":[],"operationConfigurations":[]}`)
	_, err := objects.CreateObject(context.Background(), "projects/shared/live/configuration/head", contents)
	require.NoError(t, err)
	_, _, err = store.ReadConfiguration(context.Background())
	require.Error(t, err)
}

func TestInvalidSharedConnectionDocumentsAreRejectedBeforeWriting(t *testing.T) {
	store, _ := newConfigurationStore(t)
	broker := credentialBroker(t, store, "https://broker.example.test/read")
	otherSecret, err := store.CreateApplicationSecret(context.Background(), "GITHUB_TOKEN", "not-the-scope-token")
	require.NoError(t, err)
	otherScopeStore, err := projectconfig.NewConfigurationStore(&projectconfig.ConfigurationStoreConfig{Objects: testsupport.NewObjectStore(), Scope: projectconfig.Scope{ProjectID: "other", Kind: "live"}})
	require.NoError(t, err)
	otherScopeToken, err := otherScopeStore.CreateCredentialBrokerToken(context.Background(), "other-token")
	require.NoError(t, err)

	withBroker := func(change func(*projectconfig.CredentialBrokerConfiguration)) *projectconfig.CredentialBrokerConfiguration {
		changed := *broker
		change(&changed)
		return &changed
	}
	shared := []projectconfig.ConnectionConfiguration{sharedEntry("releases", "conn-7")}
	sharedWithoutSettings := sharedEntry("releases", "conn-7")
	sharedWithoutSettings.Configuration = nil
	withoutSettings := ordinaryEntry("local")
	withoutSettings.Configuration = nil

	for name, configuration := range map[string]projectconfig.Configuration{
		"shared entry without a broker":     {Connections: shared},
		"shared entry without settings":     {Connections: []projectconfig.ConnectionConfiguration{sharedWithoutSettings}, CredentialBroker: broker},
		"ordinary entry without settings":   {Connections: []projectconfig.ConnectionConfiguration{withoutSettings}},
		"shared connection ID with a path":  {Connections: []projectconfig.ConnectionConfiguration{sharedEntry("releases", "../conn")}, CredentialBroker: broker},
		"shared connection ID with a space": {Connections: []projectconfig.ConnectionConfiguration{sharedEntry("releases", "conn 7")}, CredentialBroker: broker},
		"relative endpoint":                 {Connections: shared, CredentialBroker: withBroker(func(b *projectconfig.CredentialBrokerConfiguration) { b.Endpoint = "/read" })},
		"endpoint with another scheme":      {Connections: shared, CredentialBroker: withBroker(func(b *projectconfig.CredentialBrokerConfiguration) { b.Endpoint = "ftp://broker.example.test/read" })},
		"endpoint with credentials": {Connections: shared, CredentialBroker: withBroker(func(b *projectconfig.CredentialBrokerConfiguration) {
			b.Endpoint = "https://user:pass@broker.example.test/read"
		})},
		"endpoint with a query": {Connections: shared, CredentialBroker: withBroker(func(b *projectconfig.CredentialBrokerConfiguration) {
			b.Endpoint = "https://broker.example.test/read?token=x"
		})},
		"plain http to a public host":             {Connections: shared, CredentialBroker: withBroker(func(b *projectconfig.CredentialBrokerConfiguration) { b.Endpoint = "http://broker.example.test/read" })},
		"token reference of another secret":       {Connections: shared, CredentialBroker: withBroker(func(b *projectconfig.CredentialBrokerConfiguration) { b.TokenSecretRef = otherSecret })},
		"token reference of another scope":        {Connections: shared, CredentialBroker: withBroker(func(b *projectconfig.CredentialBrokerConfiguration) { b.TokenSecretRef = otherScopeToken })},
		"token reference without a version":       {Connections: shared, CredentialBroker: withBroker(func(b *projectconfig.CredentialBrokerConfiguration) { b.TokenSecretRef.Version = "" })},
		"token reference with a malformed digest": {Connections: shared, CredentialBroker: withBroker(func(b *projectconfig.CredentialBrokerConfiguration) { b.TokenSecretRef.Digest = "sha256:abc" })},
		"token reference outside the secret prefix": {Connections: shared, CredentialBroker: withBroker(func(b *projectconfig.CredentialBrokerConfiguration) {
			b.TokenSecretRef.Key = "projects/shared/live/connections/x"
		})},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := store.WriteConfiguration(context.Background(), 0, configuration)
			require.Error(t, err)
		})
	}
}

func TestPlainHTTPBrokerEndpointsAreAllowedOnlyForPrivateHosts(t *testing.T) {
	for name, test := range map[string]struct {
		endpoint string
		isValid  bool
	}{
		"https public host":      {endpoint: "https://broker.example.test/read", isValid: true},
		"loopback address":       {endpoint: "http://127.0.0.1:8081/read", isValid: true},
		"loopback name":          {endpoint: "http://localhost:8081/read", isValid: true},
		"private address":        {endpoint: "http://10.1.2.3:8081/read", isValid: true},
		"docker host":            {endpoint: "http://host.docker.internal:8081/read", isValid: true},
		"single label host":      {endpoint: "http://superverse-server:8081/read", isValid: true},
		"cluster service":        {endpoint: "http://credentials.superverse.svc.cluster.local:8081/read", isValid: true},
		"short cluster service":  {endpoint: "http://credentials.superverse.svc:8081/read", isValid: true},
		"public address":         {endpoint: "http://203.0.113.9/read"},
		"public name":            {endpoint: "http://broker.example.test/read"},
		"name that ends in .svc": {endpoint: "http://credentials.svc.example.test/read"},
	} {
		t.Run(name, func(t *testing.T) {
			store, _ := newConfigurationStore(t)
			_, _, err := store.WriteConfiguration(context.Background(), 0, projectconfig.Configuration{
				Connections:      []projectconfig.ConnectionConfiguration{sharedEntry("releases", "conn-7")},
				CredentialBroker: credentialBroker(t, store, test.endpoint),
			})
			if test.isValid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestCredentialBrokerTokenNameIsReservedFromApplicationSecrets(t *testing.T) {
	store, _ := newConfigurationStore(t)
	_, err := store.CreateApplicationSecret(context.Background(), "DEX_CREDENTIAL_BROKER_TOKEN", "value")
	require.Error(t, err)
	_, err = store.CreateCredentialBrokerToken(context.Background(), "")
	require.Error(t, err)
}
