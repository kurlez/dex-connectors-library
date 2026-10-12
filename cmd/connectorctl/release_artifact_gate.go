// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/superdurable/dex-connectors-library/schema"
)

// verifyReleaseAuthMethods checks the encoded release metadata before it is written: authMethods lists exactly the
// manifest's authentication methods in order, and every entry carries hostKeptCredentials as a JSON array, an empty
// one as []. A host fails closed on a release without the key, so a method that keeps nothing must still say so;
// null, a missing key, or a missing method is a build failure, never an omission.
func verifyReleaseAuthMethods(encoded []byte, manifest schema.Manifest) error {
	var published struct {
		AuthMethods *[]map[string]json.RawMessage `json:"authMethods"`
	}
	if err := json.Unmarshal(encoded, &published); err != nil {
		return fmt.Errorf("decode release artifact: %w", err)
	}
	if published.AuthMethods == nil {
		return errors.New("release artifact has no authMethods")
	}
	declared := manifest.Spec.Auth.AuthMethods()
	if len(*published.AuthMethods) != len(declared) {
		return fmt.Errorf("release artifact lists %d authMethods but the manifest declares %d", len(*published.AuthMethods), len(declared))
	}
	for index, method := range declared {
		entry := (*published.AuthMethods)[index]
		var id string
		if err := json.Unmarshal(entry["id"], &id); err != nil || id != method.ID {
			return fmt.Errorf("release artifact authMethods[%d] id is %s, want %q", index, entry["id"], method.ID)
		}
		raw, present := entry["hostKeptCredentials"]
		if !present {
			return fmt.Errorf("release artifact authMethods[%d] (%q) has no hostKeptCredentials", index, method.ID)
		}
		var fields []string
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return fmt.Errorf("release artifact authMethods[%d] (%q) hostKeptCredentials is %s, want an array", index, method.ID, raw)
		}
	}
	return nil
}
