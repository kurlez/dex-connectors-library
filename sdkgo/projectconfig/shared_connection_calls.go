// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package projectconfig

import (
	"context"
	"time"
)

const (
	// maximumIssuedCalls bounds how many provider calls a shared entry remembers a credential revision for.
	maximumIssuedCalls = 4096
	// issuedCallRetention is how long a shared entry remembers the revision it handed to a provider call.
	issuedCallRetention = 10 * time.Minute
)

type callIdentityKey struct{}

// ContextWithCallID returns a context that carries callID, the identity of one provider call across its retries.
// A shared entry remembers which credential revision it handed to the call, so that when the provider rejects the
// credential it knows which revision was rejected. Without an identity it assumes the cached revision.
func ContextWithCallID(ctx context.Context, callID string) context.Context {
	return context.WithValue(ctx, callIdentityKey{}, callID)
}

func callIDFromContext(ctx context.Context) string {
	callID, _ := ctx.Value(callIdentityKey{}).(string)
	return callID
}

// issuedCredential is the revision of the shared connection that a provider call received.
type issuedCredential struct {
	callID   string
	revision uint64
	issuedAt time.Time
}

// recordIssued remembers that the call in ctx received revision, forgetting the oldest or expired calls.
func (connection *sharedConnection) recordIssued(ctx context.Context, revision uint64) {
	callID := callIDFromContext(ctx)
	if callID == "" {
		return
	}
	now := connection.now()
	connection.mutex.Lock()
	defer connection.mutex.Unlock()
	if element, isRemembered := connection.issuedByCall[callID]; isRemembered {
		issued := element.Value.(*issuedCredential)
		issued.revision, issued.issuedAt = revision, now
		connection.issuedOrder.MoveToBack(element)
	} else {
		connection.issuedByCall[callID] = connection.issuedOrder.PushBack(&issuedCredential{callID: callID, revision: revision, issuedAt: now})
	}
	for oldest := connection.issuedOrder.Front(); oldest != nil; oldest = connection.issuedOrder.Front() {
		issued := oldest.Value.(*issuedCredential)
		if connection.issuedOrder.Len() <= maximumIssuedCalls && now.Sub(issued.issuedAt) < issuedCallRetention {
			return
		}
		connection.issuedOrder.Remove(oldest)
		delete(connection.issuedByCall, issued.callID)
	}
}

// issuedTo returns the credential that the call in ctx received. For a call that is not remembered it assumes the
// cached revision.
func (connection *sharedConnection) issuedTo(ctx context.Context) issuedCredential {
	callID := callIDFromContext(ctx)
	connection.mutex.Lock()
	defer connection.mutex.Unlock()
	if element, isRemembered := connection.issuedByCall[callID]; isRemembered {
		if issued := *element.Value.(*issuedCredential); connection.now().Sub(issued.issuedAt) < issuedCallRetention {
			return issued
		}
	}
	var issued issuedCredential
	if connection.cached != nil {
		issued.revision = connection.cached.revision
	}
	return issued
}

// supersedingCredential returns the cached credential when it has a different revision than the one issued. It has not
// expired, so the call can use it without a forced read.
func (connection *sharedConnection) supersedingCredential(issued issuedCredential) (brokerCredential, bool) {
	connection.mutex.Lock()
	defer connection.mutex.Unlock()
	if connection.cached == nil || isExpired(*connection.cached, connection.now()) || issued.revision == 0 {
		return brokerCredential{}, false
	}
	return *connection.cached, issued.revision != connection.cached.revision
}
