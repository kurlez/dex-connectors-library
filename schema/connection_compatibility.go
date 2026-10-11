// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package schema

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DecodeReleasedManifest decodes the manifest of an earlier release without validating it, because a newer
// manifest schema may reject what that release declared. Use it only to read a released connection contract.
func DecodeReleasedManifest(reader io.Reader) (Manifest, error) {
	var manifest Manifest
	if err := yaml.NewDecoder(reader).Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode released manifest: %w", err)
	}
	return manifest, nil
}

// ValidateConnectionCompatibility checks that candidate keeps every connection made with released, the latest
// release of candidate's major version, working. The connection contract is:
//
//   - every authentication method keeps its ID, type, credential fields (name, type, whether it is required and
//     its enum values) and the scopes it requests: OAuth scopes and user scopes, compared as sets or, for scopes
//     from a setting, by the setting they name, and the scopes of a client credentials or JWT bearer grant that
//     both releases declare; the default method stays the same;
//   - the connection settings, spec.configuration and each method's configuration, only gain optional fields:
//     no field is removed, renamed, retyped, made required or loses an enum value.
//
// A candidate may add an authentication method. A single-method spec.auth has the empty method ID, which a
// candidate with methods resolves through its defaultMethod. The error lists every break.
func ValidateConnectionCompatibility(released, candidate Manifest) error {
	breaks := findAuthMethodBreaks(released.Spec.Auth, candidate.Spec.Auth)
	breaks = append(breaks, findSettingBreaks("spec.configuration", released.Spec.Configuration.Fields, candidate.Spec.Configuration.Fields)...)
	if len(breaks) == 0 {
		return nil
	}
	return fmt.Errorf("connection contract changes: %s", strings.Join(breaks, "; "))
}

// connectionMethod is one authentication method with the name that break reports use for it.
type connectionMethod struct {
	label  string
	method AuthMethod
}

func findAuthMethodBreaks(released, candidate Auth) []string {
	var breaks []string
	if len(released.Methods) > 0 && len(candidate.Methods) > 0 && released.DefaultMethod != candidate.DefaultMethod {
		breaks = append(breaks, fmt.Sprintf("spec.auth.defaultMethod changes from %s to %s, which changes the method of credentials saved without one",
			released.DefaultMethod, candidate.DefaultMethod))
	}
	candidateMethods := connectionMethodsByID(candidate)
	for _, releasedMethod := range connectionMethodsInOrder(released) {
		candidateID := releasedMethod.method.ID
		if len(released.Methods) == 0 && len(candidate.Methods) > 0 {
			candidateID = candidate.DefaultMethod
			releasedMethod.label = "spec.auth, now auth method " + candidateID + ","
		}
		candidateMethod, found := candidateMethods[candidateID]
		if !found {
			breaks = append(breaks, releasedMethod.label+" is removed")
			continue
		}
		breaks = append(breaks, findMethodBreaks(releasedMethod, candidateMethod)...)
	}
	return breaks
}

func findMethodBreaks(released, candidate connectionMethod) []string {
	var breaks []string
	if released.method.Type != candidate.method.Type {
		breaks = append(breaks, fmt.Sprintf("%s changes type from %s to %s", released.label, released.method.Type, candidate.method.Type))
	}
	breaks = append(breaks, findCredentialFieldBreaks(released.label, released.method.Fields, candidate.method.Fields)...)
	breaks = append(breaks, findOAuthScopeBreaks(released.label+" OAuth scopes", oauthScopes(released.method.OAuth2), oauthScopes(candidate.method.OAuth2))...)
	breaks = append(breaks, findScopeBreaks(released.label+" OAuth user scopes", oauthUserScopes(released.method.OAuth2), oauthUserScopes(candidate.method.OAuth2))...)
	if released.method.ClientCredentials != nil && candidate.method.ClientCredentials != nil {
		breaks = append(breaks, findScopeBreaks(released.label+" client credentials scopes", released.method.ClientCredentials.Scopes, candidate.method.ClientCredentials.Scopes)...)
	}
	if releasedJWTBearer, candidateJWTBearer := jwtBearerGrant(released.method), jwtBearerGrant(candidate.method); releasedJWTBearer != nil && candidateJWTBearer != nil {
		breaks = append(breaks, findScopeBreaks(released.label+" JWT bearer scopes", releasedJWTBearer.Scopes, candidateJWTBearer.Scopes)...)
	}
	breaks = append(breaks, findSettingBreaks(released.label+" configuration", methodConfigurationFields(released.method), methodConfigurationFields(candidate.method))...)
	return breaks
}

// findCredentialFieldBreaks requires the same credential fields, because a stored credential holds exactly them.
func findCredentialFieldBreaks(label string, released, candidate []Field) []string {
	var breaks []string
	candidateFields := fieldsByName(candidate)
	for _, releasedField := range released {
		candidateField, found := candidateFields[releasedField.Name]
		if !found {
			breaks = append(breaks, fmt.Sprintf("%s credential field %s is removed", label, releasedField.Name))
			continue
		}
		breaks = append(breaks, findFieldTypeBreaks(label+" credential field", releasedField, candidateField)...)
		if releasedField.Required != candidateField.Required {
			breaks = append(breaks, fmt.Sprintf("%s credential field %s changes required from %t to %t", label, releasedField.Name, releasedField.Required, candidateField.Required))
		}
		if !hasSameStrings(releasedField.Enum, candidateField.Enum) {
			breaks = append(breaks, fmt.Sprintf("%s credential field %s changes its enum values", label, releasedField.Name))
		}
	}
	releasedFields := fieldsByName(released)
	for _, candidateField := range candidate {
		if _, found := releasedFields[candidateField.Name]; !found {
			breaks = append(breaks, fmt.Sprintf("%s adds credential field %s", label, candidateField.Name))
		}
	}
	return breaks
}

// findSettingBreaks lets settings only gain optional fields, so every stored setting stays valid.
func findSettingBreaks(label string, released, candidate []Field) []string {
	var breaks []string
	candidateFields := fieldsByName(candidate)
	for _, releasedField := range released {
		candidateField, found := candidateFields[releasedField.Name]
		if !found {
			breaks = append(breaks, fmt.Sprintf("%s field %s is removed", label, releasedField.Name))
			continue
		}
		breaks = append(breaks, findFieldTypeBreaks(label+" field", releasedField, candidateField)...)
		if !releasedField.Required && candidateField.Required {
			breaks = append(breaks, fmt.Sprintf("%s field %s becomes required", label, releasedField.Name))
		}
		if removed := missingStrings(releasedField.Enum, candidateField.Enum); len(removed) > 0 {
			breaks = append(breaks, fmt.Sprintf("%s field %s removes enum values %s", label, releasedField.Name, strings.Join(removed, ", ")))
		}
	}
	releasedFields := fieldsByName(released)
	for _, candidateField := range candidate {
		if _, found := releasedFields[candidateField.Name]; !found && candidateField.Required {
			breaks = append(breaks, fmt.Sprintf("%s adds required field %s", label, candidateField.Name))
		}
	}
	return breaks
}

func findFieldTypeBreaks(label string, released, candidate Field) []string {
	if released.Type == candidate.Type {
		return nil
	}
	return []string{fmt.Sprintf("%s %s changes type from %s to %s", label, released.Name, released.Type, candidate.Type)}
}

// findOAuthScopeBreaks compares fixed scopes as sets and setting-sourced scopes by the setting they name, because
// a connection authorized for its own setting's scopes keeps them while the setting stays the same.
func findOAuthScopeBreaks(label string, released, candidate ValueSource) []string {
	switch {
	case released.FromSetting != "" && candidate.FromSetting != "" && released.FromSetting != candidate.FromSetting:
		return []string{fmt.Sprintf("%s change from setting %s to setting %s", label, released.FromSetting, candidate.FromSetting)}
	case released.FromSetting != "" && candidate.FromSetting == "":
		return []string{fmt.Sprintf("%s change from setting %s to a fixed list", label, released.FromSetting)}
	case released.FromSetting == "" && candidate.FromSetting != "":
		return []string{fmt.Sprintf("%s change from a fixed list to setting %s", label, candidate.FromSetting)}
	}
	return findScopeBreaks(label, released.List, candidate.List)
}

func findScopeBreaks(label string, released, candidate []string) []string {
	var breaks []string
	if added := missingStrings(candidate, released); len(added) > 0 {
		breaks = append(breaks, fmt.Sprintf("%s add %s", label, strings.Join(added, ", ")))
	}
	if removed := missingStrings(released, candidate); len(removed) > 0 {
		breaks = append(breaks, fmt.Sprintf("%s remove %s", label, strings.Join(removed, ", ")))
	}
	return breaks
}

// connectionMethodsInOrder returns the declared methods, or the single spec.auth method with the empty ID.
func connectionMethodsInOrder(auth Auth) []connectionMethod {
	if len(auth.Methods) == 0 {
		return []connectionMethod{{label: "spec.auth", method: auth.singleMethod()}}
	}
	methods := make([]connectionMethod, 0, len(auth.Methods))
	for _, method := range auth.Methods {
		methods = append(methods, connectionMethod{label: "auth method " + method.ID, method: method})
	}
	return methods
}

func connectionMethodsByID(auth Auth) map[string]connectionMethod {
	methods := map[string]connectionMethod{}
	for _, method := range connectionMethodsInOrder(auth) {
		methods[method.method.ID] = method
	}
	return methods
}

func jwtBearerGrant(method AuthMethod) *JWTBearerGrant {
	if method.ServiceAccount == nil {
		return nil
	}
	return method.ServiceAccount.JWTBearer
}

func methodConfigurationFields(method AuthMethod) []Field {
	if method.Configuration == nil {
		return nil
	}
	return method.Configuration.Fields
}

func oauthScopes(oauth *OAuth2) ValueSource {
	if oauth == nil {
		return ValueSource{}
	}
	return oauth.Scopes
}

func oauthUserScopes(oauth *OAuth2) []string {
	if oauth == nil {
		return nil
	}
	return oauth.UserScopes
}

func fieldsByName(fields []Field) map[string]Field {
	byName := make(map[string]Field, len(fields))
	for _, field := range fields {
		byName[field.Name] = field
	}
	return byName
}

func hasSameStrings(left, right []string) bool {
	return len(missingStrings(left, right)) == 0 && len(missingStrings(right, left)) == 0
}

// missingStrings returns the sorted values of expected that actual lacks.
func missingStrings(expected, actual []string) []string {
	var missing []string
	for _, value := range expected {
		if !slices.Contains(actual, value) && !slices.Contains(missing, value) {
			missing = append(missing, value)
		}
	}
	sort.Strings(missing)
	return missing
}
