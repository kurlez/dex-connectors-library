// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package projectconfig

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// RelayTimestampHeader carries the Unix second at which the host signed a relayed Trigger request.
	RelayTimestampHeader = "Dex-Relay-Timestamp"
	// RelaySignatureHeader carries "v1=" and the hexadecimal HMAC-SHA256 of "v1:<timestamp>:<body>" keyed with the
	// scope token.
	RelaySignatureHeader = "Dex-Relay-Signature"
)

const (
	relaySignatureVersion = "v1"
	// relayTimestampTolerance is how far a relayed request's timestamp may be from the receiver's clock.
	relayTimestampTolerance = 5 * time.Minute
	// relayTokenRenewalInterval spaces the token rereads that signature mismatches cause, because the receiver's
	// route is public and anyone can send it a wrong signature.
	relayTokenRenewalInterval = 30 * time.Second
	// maximumRelayedTriggerRegistrationBytes is the broker's limit for one registration request.
	maximumRelayedTriggerRegistrationBytes = 4 << 10
)

var (
	// ErrRelayedTriggerNotRelayed reports that the credential broker does not relay events for the shared connection,
	// such as a connection whose credential carries its own event subscription. The connector receives its events
	// directly instead, or fails when it cannot.
	ErrRelayedTriggerNotRelayed = errors.New("the credential broker does not relay this connection's events")
	// ErrRelaySignatureInvalid reports a relayed request whose signature or timestamp is missing, malformed, stale,
	// or does not match the scope token. A receiver refuses the request.
	ErrRelaySignatureInvalid = errors.New("relayed request signature is missing, invalid or stale")
)

// relayedTriggerRegistrationStatus is the closed set of answers to a registration.
type relayedTriggerRegistrationStatus string

const (
	relayedTriggerRegistered            relayedTriggerRegistrationStatus = "REGISTERED"
	relayedTriggerConnectionNotSelected relayedTriggerRegistrationStatus = "CONNECTION_NOT_SELECTED"
	relayedTriggerNotRelayed            relayedTriggerRegistrationStatus = "NOT_RELAYED"
)

type relayedTriggerRegistrationRequest struct {
	SharedConnectionID string `json:"sharedConnectionId"`
	ConnectionName     string `json:"connectionName"`
}

// relayedTriggerRegistrationResponse decodes leniently so the broker can add fields.
type relayedTriggerRegistrationResponse struct {
	Status string `json:"status"`
}

// RegisterRelayedTrigger tells the credential broker that the shared entry key receives relayed Trigger events at
// this scope's Trigger route. registrationPath is the absolute path of the broker's registration endpoint, such as a
// connector's fixed path; it is resolved against the origin of the document's credentialBroker endpoint, so the
// request never names an address. The scope token authorizes it, with the same renewal on HTTP 401 as a credential
// read. A connector registers every time its relayed receiver starts.
//
// It returns nil once the broker stored the registration, ErrConnectionNotSelected when the scope no longer selects
// the entry's shared connection, and ErrRelayedTriggerNotRelayed when the broker does not relay the connection's
// events. ErrCredentialBrokerUnavailable reports a transport failure, a timeout, or HTTP 408, 425, 429 or 5xx, which
// the caller retries; ErrCredentialBrokerRejected reports any other HTTP status, such as 400, 409 or 413, or an
// invalid answer. A document without a credential broker, or an entry that is missing or not shared, fails without a
// request. Errors never contain the scope token or the response.
func (project *LoadedProject) RegisterRelayedTrigger(ctx context.Context, key ConnectionKey, registrationPath string) error {
	if project.broker == nil {
		return errors.New("relayed Trigger registration requires a credential broker in the project configuration")
	}
	sharedConnectionID := ""
	for _, connection := range project.Configuration.Connections {
		if connection.ConnectorID == key.ConnectorID && connection.ConnectionName == key.ConnectionName {
			sharedConnectionID = connection.SharedConnectionID
		}
	}
	if sharedConnectionID == "" {
		return fmt.Errorf("relayed Trigger registration requires a shared connection entry for %s/%s", key.ConnectorID, key.ConnectionName)
	}
	target, err := project.broker.originURL(registrationPath)
	if err != nil {
		return err
	}
	body, err := json.Marshal(relayedTriggerRegistrationRequest{SharedConnectionID: sharedConnectionID, ConnectionName: key.ConnectionName})
	if err != nil || len(body) > maximumRelayedTriggerRegistrationBytes {
		return errors.New("relayed Trigger registration request is too large or cannot be encoded")
	}
	contents, err := project.broker.exchange(ctx, target, body)
	if err != nil {
		return err
	}
	var response relayedTriggerRegistrationResponse
	if json.Unmarshal(contents, &response) != nil {
		return fmt.Errorf("%w: registration response is not a JSON object", ErrCredentialBrokerRejected)
	}
	switch relayedTriggerRegistrationStatus(response.Status) {
	case relayedTriggerRegistered:
		return nil
	case relayedTriggerConnectionNotSelected:
		return ErrConnectionNotSelected
	case relayedTriggerNotRelayed:
		return ErrRelayedTriggerNotRelayed
	default:
		return fmt.Errorf("%w: registration response status is unknown", ErrCredentialBrokerRejected)
	}
}

// VerifyRelayedRequest checks that the host signed a relayed Trigger request with this scope's token: the
// RelaySignatureHeader must be "v1=" and the hexadecimal HMAC-SHA256 of "v1:<RelayTimestampHeader>:<body>", compared
// in constant time, and the timestamp must be within 5 minutes of receivedAt. body is the raw request body.
//
// A signature that does not match the cached token makes the receiver reread the token once, as a credential read
// does after HTTP 401, so a rotated token keeps working; such rereads happen at most once every 30 seconds per
// project. It returns ErrRelaySignatureInvalid for a request to refuse, and an error that wraps
// ErrCredentialBrokerUnavailable when the token cannot be read, which the receiver answers as retryable. A document
// without a credential broker fails every request. The token never leaves this package.
func (project *LoadedProject) VerifyRelayedRequest(ctx context.Context, header http.Header, body []byte, receivedAt time.Time) error {
	if project.broker == nil {
		return errors.New("relayed Trigger verification requires a credential broker in the project configuration")
	}
	timestamp := header.Get(RelayTimestampHeader)
	signature, isVersioned := strings.CutPrefix(header.Get(RelaySignatureHeader), relaySignatureVersion+"=")
	signedAt, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || !isVersioned {
		return ErrRelaySignatureInvalid
	}
	if age := receivedAt.Sub(time.Unix(signedAt, 0)); age > relayTimestampTolerance || age < -relayTimestampTolerance {
		return ErrRelaySignatureInvalid
	}
	received, err := hex.DecodeString(signature)
	if err != nil || len(received) != sha256.Size {
		return ErrRelaySignatureInvalid
	}
	token, err := project.broker.bearerToken(ctx)
	if err != nil {
		return fmt.Errorf("%w: scope token: %w", ErrCredentialBrokerUnavailable, err)
	}
	if hmac.Equal(received, relaySignature(token, timestamp, body)) {
		return nil
	}
	renewed, isRenewed, err := project.broker.renewBearerTokenAfterMismatch(ctx, token)
	if err != nil {
		return fmt.Errorf("%w: scope token renewal: %w", ErrCredentialBrokerUnavailable, err)
	}
	if isRenewed && hmac.Equal(received, relaySignature(renewed, timestamp, body)) {
		return nil
	}
	return ErrRelaySignatureInvalid
}

func relaySignature(token string, timestamp string, body []byte) []byte {
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte(relaySignatureVersion + ":" + timestamp + ":"))
	_, _ = mac.Write(body)
	return mac.Sum(nil)
}

// originURL resolves an absolute path against the origin of the broker's endpoint.
func (broker *credentialBroker) originURL(path string) (string, error) {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "?#\\") {
		return "", errors.New("credential broker endpoint path must be an absolute path without a query or fragment")
	}
	endpoint, err := url.Parse(broker.endpoint)
	if err != nil {
		return "", errors.New("credential broker endpoint is invalid")
	}
	return (&url.URL{Scheme: endpoint.Scheme, Host: endpoint.Host, Path: path}).String(), nil
}

// renewBearerTokenAfterMismatch renews the token that a relayed request's signature did not match, unless such a
// renewal happened within relayTokenRenewalInterval. It reports whether the returned token differs from mismatched.
func (broker *credentialBroker) renewBearerTokenAfterMismatch(ctx context.Context, mismatched string) (string, bool, error) {
	broker.tokenMutex.Lock()
	now := broker.now()
	if broker.token != "" && broker.token != mismatched {
		current := broker.token
		broker.tokenMutex.Unlock()
		return current, true, nil
	}
	if !broker.lastMismatchRenewal.IsZero() && now.Sub(broker.lastMismatchRenewal) < relayTokenRenewalInterval {
		broker.tokenMutex.Unlock()
		return "", false, nil
	}
	broker.lastMismatchRenewal = now
	broker.tokenMutex.Unlock()
	renewed, err := broker.renewBearerToken(ctx, mismatched)
	if err != nil {
		return "", false, err
	}
	return renewed, renewed != mismatched, nil
}
