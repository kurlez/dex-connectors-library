// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package projectconfig

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/superdurable/dex-connectors-library/sdkgo/internal/projectload"
)

func init() {
	projectload.LoadFromObjectStore = loadProjectFromObjectStore
}

// LoadedProject contains an immutable ordinary configuration snapshot and independently resolved current credentials.
type LoadedProject struct {
	objects        ObjectStore
	scope          Scope
	configurations *ConfigurationStore
	// broker is the credential broker client, or nil when the document declares no credentialBroker.
	broker *credentialBroker
	// Connections resolves private credential material through the fixed project boundary, and a shared entry's
	// credential through the credential broker.
	Connections *ConnectionStore
	// Configuration is the exact ordinary snapshot accepted for this application instance.
	Configuration Configuration
	// Snapshot identifies the exact immutable configuration object version and digest.
	Snapshot SnapshotRef
}

// LoadFromEnvironment loads the trusted DEX_PROJECT_* deployment contract and AWS default credential chain.
// It reads only the pinned ordinary snapshot at startup; no credential read or provider refresh occurs.
// A shared entry's credential is read from the credential broker when a call first uses it, so an application
// starts while the broker is unavailable.
// Hosted storage requires an exact KMS key ARN. Local S3 endpoints require explicit local-storage opt-in.
func LoadFromEnvironment(ctx context.Context) (*LoadedProject, error) {
	scope := Scope{ProjectID: os.Getenv("DEX_PROJECT_ID"), Kind: os.Getenv("DEX_PROJECT_SCOPE")}
	if _, err := scope.Prefix(); err != nil {
		return nil, err
	}
	allowLocal := os.Getenv("DEX_PROJECT_ALLOW_LOCAL_STORAGE")
	if allowLocal != "" && allowLocal != "false" && allowLocal != "true" {
		return nil, errors.New("project local storage opt-in must be true or false")
	}
	endpoint := os.Getenv("DEX_PROJECT_STORAGE_ENDPOINT")
	if endpoint != "" && (allowLocal != "true" || !isLocalStorageEndpoint(endpoint)) {
		return nil, errors.New("custom project storage endpoint requires an explicit local fixture boundary")
	}
	if allowLocal == "true" && endpoint == "" {
		return nil, errors.New("local project storage requires an explicit local endpoint")
	}
	bucket, prefix, kmsKey := os.Getenv("DEX_PROJECT_STORAGE_BUCKET"), os.Getenv("DEX_PROJECT_STORAGE_PREFIX"), os.Getenv("DEX_PROJECT_STORAGE_KMS_KEY_ARN")
	if bucket == "" || (allowLocal != "true" && !kmsKeyARN.MatchString(kmsKey)) {
		return nil, errors.New("project storage bucket and exact hosted KMS key ARN are required")
	}
	awsConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, errors.New("project AWS credential configuration is unavailable")
	}
	client := s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		if endpoint != "" {
			options.BaseEndpoint = aws.String(endpoint)
			options.UsePathStyle = true
		}
	})
	objects, err := NewS3ObjectStore(ctx, &S3StoreConfig{Client: client, Bucket: bucket, Prefix: prefix, KMSKeyID: kmsKey, AllowUnencrypted: allowLocal == "true"})
	if err != nil {
		return nil, err
	}
	reference := SnapshotRef{Key: os.Getenv("DEX_PROJECT_CONFIG_KEY"), Version: os.Getenv("DEX_PROJECT_CONFIG_VERSION"), Digest: os.Getenv("DEX_PROJECT_CONFIG_DIGEST"), MediaType: "application/json"}
	return loadProject(ctx, objects, scope, reference, brokerClientOptions{})
}

// loadProjectFromObjectStore serves projectconfigtest.LoadProject. It skips LoadFromEnvironment's storage-target
// checks, which guard building the S3 store, so it is not exported.
func loadProjectFromObjectStore(ctx context.Context, objects ObjectStore, scope Scope, reference SnapshotRef) (*LoadedProject, error) {
	if objects == nil {
		return nil, errors.New("project object store is required")
	}
	return loadProject(ctx, objects, scope, reference, brokerClientOptions{})
}

// brokerClientOptions let tests replace the broker client's HTTP client and clock.
type brokerClientOptions struct {
	httpClient *http.Client
	now        func() time.Time
}

// loadProject reads the pinned snapshot and builds the stores.
func loadProject(ctx context.Context, objects ObjectStore, scope Scope, reference SnapshotRef, options brokerClientOptions) (*LoadedProject, error) {
	configurations, err := NewConfigurationStore(&ConfigurationStoreConfig{Objects: objects, Scope: scope})
	if err != nil {
		return nil, err
	}
	configuration, err := configurations.ReadSnapshot(ctx, reference)
	if err != nil {
		return nil, err
	}
	if options.now == nil {
		options.now = time.Now
	}
	storeConfig := &ConnectionStoreConfig{Objects: objects, Scope: scope}
	var broker *credentialBroker
	if configuration.CredentialBroker != nil {
		broker = newCredentialBroker(*configuration.CredentialBroker, configurations, options.httpClient, options.now)
		storeConfig.sharedConnections = &sharedConnectionsConfig{configuration: configuration, broker: broker, now: options.now}
	}
	connections, err := NewConnectionStore(storeConfig)
	if err != nil {
		return nil, err
	}
	return &LoadedProject{
		objects: objects, scope: scope, configurations: configurations, broker: broker,
		Connections: connections, Configuration: configuration, Snapshot: reference,
	}, nil
}

// TriggerInbox binds one Trigger binding's durable inbox in the loaded project scope.
func (project *LoadedProject) TriggerInbox(key TriggerInboxKey) (*TriggerInbox, error) {
	return NewTriggerInbox(project.objects, project.scope, key)
}

// ResolveApplicationEnvironment loads exact pinned app secrets without provider refresh or mutable configuration reads.
// Resolve and optionally Apply the result before starting application goroutines; never place returned values in a Flow.
func (project *LoadedProject) ResolveApplicationEnvironment(ctx context.Context) (ApplicationEnvironment, error) {
	return project.configurations.ResolveApplicationEnvironment(ctx, project.Configuration)
}

func isLocalStorageEndpoint(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	hostname := parsed.Hostname()
	address := net.ParseIP(hostname)
	return hostname == "localhost" || hostname == "host.docker.internal" || strings.HasSuffix(hostname, ".svc.cluster.local") || (address != nil && (address.IsLoopback() || address.IsPrivate()))
}
