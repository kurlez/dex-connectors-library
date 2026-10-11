// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package projectconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// credentialBrokerReadTimeout exceeds the 35 seconds within which the broker ends a read.
const credentialBrokerReadTimeout = 40 * time.Second

// credentialBrokerFallbackReadTimeout bounds a read that a cached, unexpired credential can stand in for.
const credentialBrokerFallbackReadTimeout = 5 * time.Second

const maximumCredentialBrokerResponseBytes = 1 << 20

// maximumBrokerObjectBytes bounds the credential and the settings objects of one response.
const maximumBrokerObjectBytes = 64 << 10

var (
	// ErrConnectionNotSelected reports that the credential broker no longer lets this scope use the shared connection,
	// and that the current configuration document does not select another one for the entry.
	ErrConnectionNotSelected = errors.New("project connection is not selected by this scope")
	// ErrCredentialBrokerUnavailable reports a credential read that failed in transit or because the broker could not
	// answer yet: a network error, a timeout, HTTP 408, 425, 429 or 5xx. The next read tries again.
	ErrCredentialBrokerUnavailable = errors.New("credential broker is unavailable")
	// ErrCredentialBrokerRejected reports a request that the broker refused (any other HTTP status, such as an invalid
	// scope token) or an answer that breaks the wire contract. Retrying cannot help until the deployment is fixed.
	ErrCredentialBrokerRejected = errors.New("credential broker rejected the request or answered invalidly")
	// ErrRejectedCredentialNotRefreshed reports that the broker returned the credential a provider rejected, unchanged,
	// because it was issued within the forced-refresh floor or has no refresh. Do not retry the provider call
	// with it now; fail the call and let the caller's retry policy try again later.
	ErrRejectedCredentialNotRefreshed = errors.New("rejected credential was not refreshed; retry later")
)

// credentialBrokerStatus is the closed set of answers to a credential read.
type credentialBrokerStatus string

const (
	credentialBrokerReady                   credentialBrokerStatus = "READY"
	credentialBrokerReauthorizationRequired credentialBrokerStatus = "REAUTHORIZATION_REQUIRED"
	credentialBrokerConnectionNotSelected   credentialBrokerStatus = "CONNECTION_NOT_SELECTED"
)

type credentialBrokerRequest struct {
	SharedConnectionID string `json:"sharedConnectionId"`
	RejectedRevision   uint64 `json:"rejectedRevision,omitempty"`
}

// credentialBrokerResponse decodes leniently so the broker can add fields without breaking older applications.
type credentialBrokerResponse struct {
	Status     string          `json:"status"`
	Credential json.RawMessage `json:"credential"`
	Settings   json.RawMessage `json:"settings"`
	Revision   uint64          `json:"revision"`
	IssuedAt   *time.Time      `json:"issuedAt"`
	ExpiresAt  *time.Time      `json:"expiresAt"`
}

// brokerCredential is a validated READY answer. Its fields are private provider-boundary bytes.
type brokerCredential struct {
	credential json.RawMessage
	revision   uint64
	issuedAt   *time.Time
	expiresAt  *time.Time
	// readAt is when this process received the answer.
	readAt time.Time
}

// credentialBrokerAnswer is a validated answer; settings may accompany any status.
type credentialBrokerAnswer struct {
	status     credentialBrokerStatus
	credential brokerCredential
	settings   json.RawMessage
}

// credentialBroker is the HTTP client of the broker's credential read endpoint.
type credentialBroker struct {
	endpoint       string
	httpClient     *http.Client
	configurations *ConfigurationStore
	tokenReference ApplicationSecretRef

	tokenMutex sync.Mutex
	token      string
}

func newCredentialBroker(configuration CredentialBrokerConfiguration, configurations *ConfigurationStore, httpClient *http.Client) *credentialBroker {
	if httpClient == nil {
		httpClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &credentialBroker{endpoint: configuration.Endpoint, httpClient: httpClient, configurations: configurations, tokenReference: configuration.TokenSecretRef}
}

// read asks the broker for one shared connection. A timeout is ErrCredentialBrokerUnavailable.
// Errors never contain the scope token or the response.
func (broker *credentialBroker) read(ctx context.Context, sharedConnectionID string, rejectedRevision uint64, timeout time.Duration) (credentialBrokerAnswer, error) {
	token, err := broker.bearerToken(ctx)
	if err != nil {
		return credentialBrokerAnswer{}, fmt.Errorf("credential broker token: %w", err)
	}
	body, err := json.Marshal(credentialBrokerRequest{SharedConnectionID: sharedConnectionID, RejectedRevision: rejectedRevision})
	if err != nil {
		return credentialBrokerAnswer{}, errors.New("credential broker request encoding failed")
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, broker.endpoint, bytes.NewReader(body))
	if err != nil {
		return credentialBrokerAnswer{}, fmt.Errorf("%w: request could not be built", ErrCredentialBrokerRejected)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := broker.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return credentialBrokerAnswer{}, ctx.Err()
		}
		return credentialBrokerAnswer{}, fmt.Errorf("%w: %w", ErrCredentialBrokerUnavailable, err)
	}
	contents, readErr := io.ReadAll(io.LimitReader(response.Body, maximumCredentialBrokerResponseBytes+1))
	closeErr := response.Body.Close()
	switch {
	case response.StatusCode == http.StatusOK:
	case isTransientBrokerStatus(response.StatusCode):
		return credentialBrokerAnswer{}, fmt.Errorf("%w: HTTP %d", ErrCredentialBrokerUnavailable, response.StatusCode)
	default:
		return credentialBrokerAnswer{}, fmt.Errorf("%w: HTTP %d", ErrCredentialBrokerRejected, response.StatusCode)
	}
	if readErr != nil || closeErr != nil {
		if ctx.Err() != nil {
			return credentialBrokerAnswer{}, ctx.Err()
		}
		return credentialBrokerAnswer{}, fmt.Errorf("%w: response could not be read", ErrCredentialBrokerUnavailable)
	}
	return decodeCredentialBrokerAnswer(contents)
}

// bearerToken reads the pinned scope token once; a failure is not cached.
func (broker *credentialBroker) bearerToken(ctx context.Context) (string, error) {
	broker.tokenMutex.Lock()
	defer broker.tokenMutex.Unlock()
	if broker.token != "" {
		return broker.token, nil
	}
	token, err := broker.configurations.readCredentialBrokerToken(ctx, broker.tokenReference)
	if err != nil {
		return "", err
	}
	broker.token = token
	return token, nil
}

func decodeCredentialBrokerAnswer(contents []byte) (credentialBrokerAnswer, error) {
	invalid := func(reason string) (credentialBrokerAnswer, error) {
		return credentialBrokerAnswer{}, fmt.Errorf("%w: %s", ErrCredentialBrokerRejected, reason)
	}
	if len(contents) > maximumCredentialBrokerResponseBytes {
		return invalid("response is too large")
	}
	var response credentialBrokerResponse
	if json.Unmarshal(contents, &response) != nil {
		return invalid("response is not a JSON object")
	}
	answer := credentialBrokerAnswer{status: credentialBrokerStatus(response.Status)}
	if len(response.Settings) > 0 && len(response.Settings) <= maximumBrokerObjectBytes && isJSONObject(response.Settings) {
		answer.settings = response.Settings
	}
	switch answer.status {
	case credentialBrokerReauthorizationRequired, credentialBrokerConnectionNotSelected:
		return answer, nil
	case credentialBrokerReady:
	default:
		return invalid("response status is unknown")
	}
	if len(response.Credential) == 0 || len(response.Credential) > maximumBrokerObjectBytes || !isJSONObject(response.Credential) {
		return invalid("response credential is not a JSON object")
	}
	if response.Revision == 0 {
		return invalid("response revision is missing")
	}
	answer.credential = brokerCredential{credential: response.Credential, revision: response.Revision, issuedAt: response.IssuedAt, expiresAt: response.ExpiresAt}
	return answer, nil
}

func isTransientBrokerStatus(statusCode int) bool {
	return statusCode == http.StatusRequestTimeout || statusCode == http.StatusTooEarly || statusCode == http.StatusTooManyRequests || statusCode >= http.StatusInternalServerError
}
