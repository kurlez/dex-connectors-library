// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package projectconfig

import (
	"container/list"
	"context"
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
	// configurations rereads the document's credentialBroker to renew the scope token.
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
			broker: broker, now: config.now, readSlot: make(chan struct{}, 1), sharedConnectionID: connection.SharedConnectionID,
			issuedByCall: map[string]*list.Element{}, issuedOrder: list.New(),
		}
	}
	return shared
}

// sharedConnection caches the broker's credential for one entry. It never touches connections/* or refreshes.
type sharedConnection struct {
	broker *credentialBroker
	now    func() time.Time
	// sharedConnectionID is the loaded document's selection, which the process keeps until it ends.
	sharedConnectionID string
	// readSlot admits one read at a time; waiters honor their own context and then reuse its result.
	readSlot chan struct{}

	mutex  sync.Mutex
	cached *brokerCredential
	// nextReadNotBefore defers the next read of a cached credential.
	nextReadNotBefore time.Time
	recentFailure     error
	recentFailureAt   time.Time
	// issuedByCall and issuedOrder remember the revision each provider call received, oldest call first.
	issuedByCall map[string]*list.Element
	issuedOrder  *list.List
}

// credential returns the cached credential for the call in ctx and remembers its revision for that call.
func (connection *sharedConnection) credential(ctx context.Context) (brokerCredential, error) {
	credential, err := connection.currentCredential(ctx)
	if err == nil {
		connection.recordIssued(ctx, credential.revision)
	}
	return credential, err
}

// currentCredential reads the broker first when the cached credential is stale. A caller that finds a stale
// credential that has not expired never waits for a read in flight.
func (connection *sharedConnection) currentCredential(ctx context.Context) (brokerCredential, error) {
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

// credentialAfterRejection serves a call whose credential a provider rejected. When the cache holds a different
// revision than the call received, it returns that one without a forced read. Otherwise it reads again with the
// rejected revision, and an unchanged revision fails the call.
func (connection *sharedConnection) credentialAfterRejection(ctx context.Context) (brokerCredential, error) {
	issued := connection.issuedTo(ctx)
	if credential, isSuperseding := connection.supersedingCredential(issued); isSuperseding {
		connection.recordIssued(ctx, credential.revision)
		return credential, nil
	}
	if err := connection.acquireReadSlot(ctx); err != nil {
		return brokerCredential{}, err
	}
	defer connection.releaseReadSlot()
	if credential, isSuperseding := connection.supersedingCredential(issued); isSuperseding {
		connection.recordIssued(ctx, credential.revision)
		return credential, nil
	}
	credential, err := connection.read(ctx, issued.revision)
	if err != nil {
		return brokerCredential{}, err
	}
	if issued.revision != 0 && credential.revision == issued.revision {
		return brokerCredential{}, ErrRejectedCredentialNotRefreshed
	}
	connection.recordIssued(ctx, credential.revision)
	return credential, nil
}

// read asks the broker. CONNECTION_NOT_SELECTED is terminal: the process keeps the selection it was loaded with, so it
// never pairs another connection's credential with this entry's settings.
func (connection *sharedConnection) read(ctx context.Context, rejectedRevision uint64) (brokerCredential, error) {
	answer, err := connection.broker.read(ctx, connection.sharedConnectionID, rejectedRevision)
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
	default:
		connection.forget()
		return brokerCredential{}, ErrConnectionNotSelected
	}
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
