// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package schema

import "strings"

// HostKeptCredentials returns the credential fields of method that serve only to refresh or mint its access token,
// in the order of the method's fields. A host that keeps a shared connection refreshes and mints its credential
// itself and serves the credential to applications without these fields, so that code an application runs can
// never refresh, mint, or rotate the shared credential. They are:
//
//   - for an oauth2 method, its clientSecretCredential, every refresh.grants refreshTokenCredential, and every field
//     that a credential mapping of the method or of a refresh grant fills from a refresh_token member, such as
//     refresh_token or authed_user.refresh_token;
//   - for a clientCredentials grant, its clientSecretCredential;
//   - for a serviceAccount.jwtBearer grant, its keyCredential and the field that its privateKey names with
//     fromCredential.
//
// The library is the only owner of this rule: connector code generation decodes shared credentials with it, and
// release metadata publishes its result for each method as hostKeptCredentials.
func HostKeptCredentials(method AuthMethod) []string {
	kept := map[string]bool{}
	if oauth := method.OAuth2; oauth != nil {
		kept[oauth.ClientSecretCredential] = true
		mappings := oauth.CredentialMappings
		if oauth.Refresh != nil {
			for _, grant := range oauth.Refresh.Grants {
				kept[grant.RefreshTokenCredential] = true
				mappings = append(append([]OAuthCredentialMapping(nil), mappings...), grant.CredentialMappings...)
			}
		}
		for _, mapping := range mappings {
			if mapping.Source == "refresh_token" || strings.HasSuffix(mapping.Source, ".refresh_token") {
				kept[mapping.Credential] = true
			}
		}
	}
	if grant := method.ClientCredentials; grant != nil {
		kept[grant.ClientSecretCredential] = true
	}
	if grant := jwtBearerGrant(method); grant != nil {
		kept[grant.KeyCredential] = true
		kept[grant.PrivateKey.FromCredential] = true
	}
	var fields []string
	for _, field := range method.Fields {
		if kept[field.Name] {
			fields = append(fields, field.Name)
		}
	}
	return fields
}

// AuthMethods returns the declared authentication methods, or spec.auth as one method with the empty ID.
func (auth Auth) AuthMethods() []AuthMethod {
	if len(auth.Methods) == 0 {
		return []AuthMethod{auth.singleMethod()}
	}
	return auth.Methods
}
