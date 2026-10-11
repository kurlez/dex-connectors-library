// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package schema

import "fmt"

// isOAuthEndpointSource reports whether an OAuth or OpenID Connect endpoint is exactly one of a fixed HTTPS URL and a
// {fromSetting} reference. A setting reference carries no scheme rule: the connection owner enters the value, and
// the consumer that reads it decides whether plain HTTP is acceptable.
func isOAuthEndpointSource(endpoint ValueSource) bool {
	return endpoint.sourceCount() == 1 && len(endpoint.Values) == 0 && (endpoint.FromSetting != "" || isHTTPSURL(endpoint.Value))
}

// isOAuthScopesSource reports whether OAuth scopes are exactly one of a non-empty fixed list and a {fromSetting}
// reference.
func isOAuthScopesSource(scopes ValueSource) bool {
	return scopes.sourceCount() == 1 && len(scopes.Values) == 0 && (scopes.FromSetting != "" || len(scopes.List) > 0)
}

// validateOAuthEndpointSetting checks that a setting reference names a required url field. A fixed URL has nothing
// to resolve; isOAuthEndpointSource already requires it to be HTTPS.
func validateOAuthEndpointSetting(label string, endpoint ValueSource, settings map[string]Field) []string {
	if endpoint.FromSetting == "" {
		return nil
	}
	return validateSettingReference(label, endpoint.FromSetting, "url", settings)
}

// validateOAuthScopesSetting checks that a setting reference names a required stringList field.
func validateOAuthScopesSetting(label string, scopes ValueSource, settings map[string]Field) []string {
	if scopes.FromSetting == "" {
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
