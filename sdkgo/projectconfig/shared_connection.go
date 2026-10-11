// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package projectconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/superdurable/dex-connectors-library/sdkgo/internal/refreshtiming"
)

const (
	// staticCredentialRereadInterval is how long a credential without an expiry is used before it is read again.
	staticCredentialRereadInterval = 15 * time.Minute
	// staleRereadSuppression spaces reads of a credential that is still stale after a read.
	staleRereadSuppression = time.Second
	// brokerRetryInterval is how long a cached credential is used after the broker was unavailable.
	brokerRetryInterval = 10 * time.Second
	// recentFailureReuse is how long callers share a failed read instead of repeating it.
	recentFailureReuse = 2 * time.Second
)

// sharedConnectionsConfig routes the document's shared entries to the credential broker.
type sharedConnectionsConfig struct {
	// configuration is the loaded document: its credentialBroker and its shared entries.
	configuration Configuration
	// configurations rereads the document after CONNECTION_NOT_SELECTED.
	configurations *ConfigurationStore
	httpClient     *http.Client
	now            func() time.Time
}

// sharedConnections holds one sharedConnection for each shared entry of the loaded document.
type sharedConnections struct {
	entries map[ConnectionKey]*sharedConnection
}

func newSharedConnections(config *sharedConnectionsConfig) *sharedConnections {
	if config.now == nil {
		config.now = time.Now
	}
	broker := newCredentialBroker(*config.configuration.CredentialBroker, config.configurations, config.httpClient)
	shared := &sharedConnections{entries: map[ConnectionKey]*sharedConnection{}}
	for _, connection := range config.configuration.Connections {
		if connection.SharedConnectionID == "" {
			continue
		}
		key := ConnectionKey{ConnectorID: connection.ConnectorID, ConnectionName: connection.ConnectionName}
		shared.entries[key] = &sharedConnection{
			broker: broker, configurations: config.configurations, key: key, now: config.now,
			readSlot: make(chan struct{}, 1), sharedConnectionID: connection.SharedConnectionID,
		}
	}
	return shared
}

// sharedConnection caches the broker's credential for one entry. It never touches connections/* or refreshes.
type sharedConnection struct {
	broker         *credentialBroker
	configurations *ConfigurationStore
	key            ConnectionKey
	now            func() time.Time
	// readSlot admits one read at a time; waiters honor their own context and then reuse its result.
	readSlot chan struct{}

	mutex              sync.Mutex
	sharedConnectionID string
	cached             *brokerCredential
	// nextReadNotBefore defers the next read of a cached credential.
	nextReadNotBefore time.Time
	recentFailure     error
	recentFailureAt   time.Time
}

// credential returns the cached credential, reading the broker first when it is stale. A caller that finds a stale
// credential that has not expired never waits for a read in flight.
func (connection *sharedConnection) credential(ctx context.Context) (brokerCredential, error) {
	if credential, isUsable := connection.usableCredential(); isUsable {
		return credential, nil
	}
	if stale, hasUnexpiredCredential := connection.unexpiredCredential(); hasUnexpiredCredential {
		if !connection.tryAcquireReadSlot() {
			return stale, nil
		}
	} else if err := connection.acquireReadSlot(ctx); err != nil {
		return brokerCredential{}, err
	}
	defer connection.releaseReadSlot()
	if credential, isUsable := connection.usableCredential(); isUsable {
		return credential, nil
	}
	if err := connection.sharedRecentFailure(); err != nil {
		return brokerCredential{}, err
	}
	credential, err := connection.read(ctx, 0)
	if errors.Is(err, ErrCredentialBrokerUnavailable) {
		if cached, hasUnexpiredCredential := connection.unexpiredCredential(); hasUnexpiredCredential {
			connection.deferNextRead(brokerRetryInterval)
			return cached, nil
		}
	}
	if err != nil {
		connection.rememberFailure(ctx, err)
		return brokerCredential{}, err
	}
	return credential, nil
}

// credentialAfterRejection reads again with the cached revision as rejected. An unchanged revision fails.
// The caller does not say which revision it used, so a concurrent refresh makes this imprecise.
func (connection *sharedConnection) credentialAfterRejection(ctx context.Context) (brokerCredential, error) {
	rejectedRevision := connection.cachedRevision()
	if err := connection.acquireReadSlot(ctx); err != nil {
		return brokerCredential{}, err
	}
	defer connection.releaseReadSlot()
	if connection.cachedRevision() != rejectedRevision {
		if credential, isUsable := connection.usableCredential(); isUsable {
			return credential, nil
		}
	}
	credential, err := connection.read(ctx, rejectedRevision)
	if err != nil {
		return brokerCredential{}, err
	}
	if rejectedRevision != 0 && credential.revision == rejectedRevision {
		return brokerCredential{}, ErrRejectedCredentialNotRefreshed
	}
	return credential, nil
}

// prime reads at load and returns the settings. A status other than READY is reported on first use.
func (connection *sharedConnection) prime(ctx context.Context) (json.RawMessage, error) {
	if err := connection.acquireReadSlot(ctx); err != nil {
		return nil, err
	}
	defer connection.releaseReadSlot()
	answer, err := connection.broker.read(ctx, connection.currentSharedConnectionID(), 0)
	if err != nil {
		return nil, err
	}
	if answer.status == credentialBrokerReady && !isExpired(answer.credential, connection.now()) {
		connection.remember(answer.credential)
	}
	return answer.settings, nil
}

// read asks the broker. CONNECTION_NOT_SELECTED rereads the document once and follows a changed selection.
func (connection *sharedConnection) read(ctx context.Context, rejectedRevision uint64) (brokerCredential, error) {
	hasRereadConfiguration := false
	for {
		answer, err := connection.broker.read(ctx, connection.currentSharedConnectionID(), rejectedRevision)
		if err != nil {
			return brokerCredential{}, err
		}
		switch answer.status {
		case credentialBrokerReady:
			if isExpired(answer.credential, connection.now()) {
				return brokerCredential{}, fmt.Errorf("%w: the credential has already expired", ErrCredentialBrokerUnavailable)
			}
			return connection.remember(answer.credential), nil
		case credentialBrokerReauthorizationRequired:
			connection.forget()
			return brokerCredential{}, ErrReauthorizationRequired
		}
		connection.forget()
		if hasRereadConfiguration {
			return brokerCredential{}, ErrConnectionNotSelected
		}
		hasRereadConfiguration = true
		isSelectionChanged, err := connection.rereadSharedConnectionID(ctx)
		if err != nil {
			return brokerCredential{}, fmt.Errorf("%w: rereading the configuration document: %w", ErrConnectionNotSelected, err)
		}
		if !isSelectionChanged {
			return brokerCredential{}, ErrConnectionNotSelected
		}
		rejectedRevision = 0
	}
}

// rereadSharedConnectionID takes only this entry's sharedConnectionId from the current document.
func (connection *sharedConnection) rereadSharedConnectionID(ctx context.Context) (bool, error) {
	configuration, _, err := connection.configurations.ReadConfiguration(ctx)
	if err != nil {
		return false, err
	}
	for _, entry := range configuration.Connections {
		if entry.ConnectorID != connection.key.ConnectorID || entry.ConnectionName != connection.key.ConnectionName {
			continue
		}
		connection.mutex.Lock()
		defer connection.mutex.Unlock()
		if entry.SharedConnectionID == "" || entry.SharedConnectionID == connection.sharedConnectionID {
			return false, nil
		}
		connection.sharedConnectionID = entry.SharedConnectionID
		return true, nil
	}
	return false, nil
}

// remember caches a READY credential and returns it with its receipt time.
func (connection *sharedConnection) remember(credential brokerCredential) brokerCredential {
	now := connection.now()
	credential.readAt = now
	connection.mutex.Lock()
	defer connection.mutex.Unlock()
	connection.cached = &credential
	connection.recentFailure = nil
	connection.nextReadNotBefore = time.Time{}
	if isStale(credential, now) {
		connection.nextReadNotBefore = now.Add(staleRereadSuppression)
	}
	return credential
}

func (connection *sharedConnection) forget() {
	connection.mutex.Lock()
	defer connection.mutex.Unlock()
	connection.cached = nil
}

// usableCredential returns the cached credential when it is unexpired and fresh or not yet due for a read.
func (connection *sharedConnection) usableCredential() (brokerCredential, bool) {
	connection.mutex.Lock()
	defer connection.mutex.Unlock()
	now := connection.now()
	if connection.cached == nil || isExpired(*connection.cached, now) {
		return brokerCredential{}, false
	}
	if isStale(*connection.cached, now) && !now.Before(connection.nextReadNotBefore) {
		return brokerCredential{}, false
	}
	return *connection.cached, true
}

func (connection *sharedConnection) unexpiredCredential() (brokerCredential, bool) {
	connection.mutex.Lock()
	defer connection.mutex.Unlock()
	if connection.cached == nil || isExpired(*connection.cached, connection.now()) {
		return brokerCredential{}, false
	}
	return *connection.cached, true
}

func (connection *sharedConnection) cachedRevision() uint64 {
	connection.mutex.Lock()
	defer connection.mutex.Unlock()
	if connection.cached == nil {
		return 0
	}
	return connection.cached.revision
}

func (connection *sharedConnection) currentSharedConnectionID() string {
	connection.mutex.Lock()
	defer connection.mutex.Unlock()
	return connection.sharedConnectionID
}

func (connection *sharedConnection) deferNextRead(interval time.Duration) {
	connection.mutex.Lock()
	defer connection.mutex.Unlock()
	connection.nextReadNotBefore = connection.now().Add(interval)
}

// rememberFailure lets queued callers share a failed read; a caller's cancellation is not one.
func (connection *sharedConnection) rememberFailure(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	connection.mutex.Lock()
	defer connection.mutex.Unlock()
	connection.recentFailure, connection.recentFailureAt = err, connection.now()
}

func (connection *sharedConnection) sharedRecentFailure() error {
	connection.mutex.Lock()
	defer connection.mutex.Unlock()
	if connection.recentFailure != nil && connection.now().Sub(connection.recentFailureAt) < recentFailureReuse {
		return connection.recentFailure
	}
	return nil
}

func (connection *sharedConnection) acquireReadSlot(ctx context.Context) error {
	select {
	case connection.readSlot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (connection *sharedConnection) tryAcquireReadSlot() bool {
	select {
	case connection.readSlot <- struct{}{}:
		return true
	default:
		return false
	}
}

func (connection *sharedConnection) releaseReadSlot() { <-connection.readSlot }

// isStale reports whether the credential is within the proportional skew of expiry or, without one, 15 minutes old.
func isStale(credential brokerCredential, now time.Time) bool {
	if credential.expiresAt == nil {
		return now.Sub(credential.readAt) >= staticCredentialRereadInterval
	}
	return refreshtiming.IsExpiryWithinSkew(credential.issuedAt, credential.expiresAt, now)
}

func isExpired(credential brokerCredential, now time.Time) bool {
	return credential.expiresAt != nil && !now.Before(*credential.expiresAt)
}

// loadSharedConnectionSettings fills each shared entry's settings from the broker and caches its first credential.
func (project *LoadedProject) loadSharedConnectionSettings(ctx context.Context) error {
	if project.Connections.shared == nil {
		return nil
	}
	for index, entry := range project.Configuration.Connections {
		if entry.SharedConnectionID == "" {
			continue
		}
		key := ConnectionKey{ConnectorID: entry.ConnectorID, ConnectionName: entry.ConnectionName}
		settings, err := project.Connections.shared.entries[key].prime(ctx)
		if err != nil {
			return fmt.Errorf("shared connection %q of connector %q: %w", entry.ConnectionName, entry.ConnectorID, err)
		}
		if len(settings) == 0 {
			settings = json.RawMessage(`{}`)
		}
		project.Configuration.Connections[index].Configuration = settings
	}
	return nil
}
