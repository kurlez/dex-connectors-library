// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package schema

import (
	"bytes"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// fromSettingKey is the only key of a setting reference, such as {fromSetting: tokenEndpoint}.
const fromSettingKey = "fromSetting"

// OAuthEndpoint is one OAuth or OpenID Connect URL. A manifest writes it either as a fixed HTTPS URL
// string or as a reference to a connection setting, {fromSetting: <configuration field name>}. A fixed
// URL must be HTTPS. A setting reference carries no scheme rule: the connection owner enters the value,
// and the consumer that reads it decides whether plain HTTP is acceptable.
type OAuthEndpoint struct {
	// URL is the fixed HTTPS URL. It is empty when FromSetting is set.
	URL string
	// FromSetting names the required url configuration field that holds the endpoint. It is empty when URL is set.
	FromSetting string
}

// IsFromSetting reports whether the connection settings supply the endpoint.
func (endpoint OAuthEndpoint) IsFromSetting() bool { return endpoint.FromSetting != "" }

// hasValidSource reports whether the endpoint is exactly one of a fixed HTTPS URL and a setting reference.
func (endpoint OAuthEndpoint) hasValidSource() bool {
	if endpoint.IsFromSetting() {
		return endpoint.URL == ""
	}
	return isHTTPSURL(endpoint.URL)
}

// UnmarshalYAML reads a fixed URL string or a {fromSetting} mapping.
func (endpoint *OAuthEndpoint) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.MappingNode {
		settingName, err := decodeSettingReference(node)
		if err != nil {
			return err
		}
		*endpoint = OAuthEndpoint{FromSetting: settingName}
		return nil
	}
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: an OAuth endpoint must be a URL string or a {%s} mapping", node.Line, fromSettingKey)
	}
	*endpoint = OAuthEndpoint{URL: node.Value}
	return nil
}

// MarshalYAML writes the fixed URL string or the {fromSetting} mapping.
func (endpoint OAuthEndpoint) MarshalYAML() (any, error) {
	if endpoint.IsFromSetting() {
		return map[string]string{fromSettingKey: endpoint.FromSetting}, nil
	}
	return endpoint.URL, nil
}

// MarshalJSON writes a fixed endpoint as the plain URL string, as before setting references existed.
func (endpoint OAuthEndpoint) MarshalJSON() ([]byte, error) {
	if endpoint.IsFromSetting() {
		return json.Marshal(map[string]string{fromSettingKey: endpoint.FromSetting})
	}
	return json.Marshal(endpoint.URL)
}

// UnmarshalJSON reads a fixed URL string or a {fromSetting} object.
func (endpoint *OAuthEndpoint) UnmarshalJSON(contents []byte) error {
	trimmed := bytes.TrimSpace(contents)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		settingName, err := decodeSettingReferenceJSON(trimmed)
		if err != nil {
			return err
		}
		*endpoint = OAuthEndpoint{FromSetting: settingName}
		return nil
	}
	var url string
	if err := json.Unmarshal(trimmed, &url); err != nil {
		return fmt.Errorf("an OAuth endpoint must be a URL string or a {%s} object", fromSettingKey)
	}
	*endpoint = OAuthEndpoint{URL: url}
	return nil
}

// OAuthScopes is the scope list of one OAuth flow. A manifest writes it either as a fixed list or as a
// reference to a connection setting, {fromSetting: <configuration field name>}, whose stringList value
// the connection owner enters.
type OAuthScopes struct {
	// Values are the fixed scopes. They are empty when FromSetting is set.
	Values []string
	// FromSetting names the required stringList configuration field that holds the scopes. It is empty when Values is set.
	FromSetting string
}

// IsFromSetting reports whether the connection settings supply the scopes.
func (scopes OAuthScopes) IsFromSetting() bool { return scopes.FromSetting != "" }

// hasValidSource reports whether the scopes are exactly one of a non-empty fixed list and a setting reference.
func (scopes OAuthScopes) hasValidSource() bool {
	if scopes.IsFromSetting() {
		return len(scopes.Values) == 0
	}
	return len(scopes.Values) > 0
}

// UnmarshalYAML reads a fixed list or a {fromSetting} mapping.
func (scopes *OAuthScopes) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.MappingNode:
		settingName, err := decodeSettingReference(node)
		if err != nil {
			return err
		}
		*scopes = OAuthScopes{FromSetting: settingName}
		return nil
	case yaml.SequenceNode:
		var values []string
		if err := node.Decode(&values); err != nil {
			return err
		}
		*scopes = OAuthScopes{Values: values}
		return nil
	}
	return fmt.Errorf("line %d: OAuth scopes must be a list of strings or a {%s} mapping", node.Line, fromSettingKey)
}

// MarshalYAML writes the fixed list or the {fromSetting} mapping.
func (scopes OAuthScopes) MarshalYAML() (any, error) {
	if scopes.IsFromSetting() {
		return map[string]string{fromSettingKey: scopes.FromSetting}, nil
	}
	return scopes.Values, nil
}

// MarshalJSON writes fixed scopes as the plain array, as before setting references existed.
func (scopes OAuthScopes) MarshalJSON() ([]byte, error) {
	if scopes.IsFromSetting() {
		return json.Marshal(map[string]string{fromSettingKey: scopes.FromSetting})
	}
	return json.Marshal(scopes.Values)
}

// UnmarshalJSON reads a fixed array or a {fromSetting} object.
func (scopes *OAuthScopes) UnmarshalJSON(contents []byte) error {
	trimmed := bytes.TrimSpace(contents)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		settingName, err := decodeSettingReferenceJSON(trimmed)
		if err != nil {
			return err
		}
		*scopes = OAuthScopes{FromSetting: settingName}
		return nil
	}
	var values []string
	if err := json.Unmarshal(trimmed, &values); err != nil {
		return fmt.Errorf("OAuth scopes must be an array of strings or a {%s} object", fromSettingKey)
	}
	*scopes = OAuthScopes{Values: values}
	return nil
}

// decodeSettingReference reads a mapping that holds exactly the fromSetting key and a non-empty name.
func decodeSettingReference(node *yaml.Node) (string, error) {
	if len(node.Content) != 2 || node.Content[0].Value != fromSettingKey || node.Content[1].Kind != yaml.ScalarNode || node.Content[1].Value == "" {
		return "", fmt.Errorf("line %d: a setting reference must be exactly {%s: <configuration field name>}", node.Line, fromSettingKey)
	}
	return node.Content[1].Value, nil
}

func decodeSettingReferenceJSON(contents []byte) (string, error) {
	var reference struct {
		FromSetting string `json:"fromSetting"`
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&reference); err != nil || reference.FromSetting == "" {
		return "", fmt.Errorf("a setting reference must be exactly {%s: <configuration field name>}", fromSettingKey)
	}
	return reference.FromSetting, nil
}

// validateOAuthEndpointSetting checks that a setting reference names a required url field. A fixed URL
// has nothing to resolve; hasValidSource already requires it to be HTTPS.
func validateOAuthEndpointSetting(label string, endpoint OAuthEndpoint, settings map[string]Field) []string {
	if !endpoint.IsFromSetting() {
		return nil
	}
	return validateSettingReference(label, endpoint.FromSetting, "url", settings)
}

// validateOAuthScopesSetting checks that a setting reference names a required stringList field.
func validateOAuthScopesSetting(label string, scopes OAuthScopes, settings map[string]Field) []string {
	if !scopes.IsFromSetting() {
		return nil
	}
	return validateSettingReference(label, scopes.FromSetting, "stringList", settings)
}

// validateSettingReference requires the named configuration field to exist, have fieldType, and be
// required, so the consumer always finds a value when it builds the authorization request.
func validateSettingReference(label string, settingName string, fieldType string, settings map[string]Field) []string {
	field, found := settings[settingName]
	switch {
	case !found:
		return []string{fmt.Sprintf("%s fromSetting %s must name a configuration field of this auth method", label, settingName)}
	case field.Type != fieldType:
		return []string{fmt.Sprintf("%s fromSetting %s must name a %s configuration field", label, settingName, fieldType)}
	case !field.Required:
		return []string{fmt.Sprintf("%s fromSetting %s must name a required configuration field", label, settingName)}
	}
	return nil
}

// authMethodSettings indexes the configuration fields an auth method may name in a fromSetting reference:
// the spec-wide fields and the method's own.
func authMethodSettings(specFields []Field, methodConfiguration *Configuration) map[string]Field {
	settings := make(map[string]Field, len(specFields))
	for _, field := range specFields {
		settings[field.Name] = field
	}
	if methodConfiguration != nil {
		for _, field := range methodConfiguration.Fields {
			settings[field.Name] = field
		}
	}
	return settings
}
