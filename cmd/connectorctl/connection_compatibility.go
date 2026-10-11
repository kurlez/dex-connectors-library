// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"strings"

	"github.com/superdurable/dex-connectors-library/schema"
)

// validateConnectionCompatibility checks that a pending release or candidate of targetVersion keeps the connections
// of latestRelease, the connector's latest release reachable from main. Versions only move forward, so latestRelease
// is the latest release of targetVersion's major version exactly when the majors match; a major version without a
// release has no contract yet.
func validateConnectionCompatibility(repositoryRoot string, entry connectorDirectoryEntry, latestRelease reachableConnectorRelease, targetVersion string) error {
	if latestRelease.Version == "" {
		return nil
	}
	targetMajorVersion := connectorVersionParts(targetVersion)[0]
	if connectorVersionParts(latestRelease.Version)[0] != targetMajorVersion {
		return nil
	}
	releasedContents, err := gitOutput(repositoryRoot, "show", latestRelease.SourceCommit+":"+entry.ManifestPath)
	if err != nil {
		return fmt.Errorf("read the manifest of %s %s: %w", entry.Directory, latestRelease.Version, err)
	}
	released, err := schema.DecodeReleasedManifest(strings.NewReader(releasedContents))
	if err != nil {
		return fmt.Errorf("%s %s: %w", entry.Directory, latestRelease.Version, err)
	}
	if err := schema.ValidateConnectionCompatibility(released, entry.Manifest); err != nil {
		return fmt.Errorf("%s %s breaks connections made with %s, so it must be v%d.0.0 (see docs/versioning-and-releases.md#connection-compatibility): %w",
			entry.Directory, targetVersion, latestRelease.Version, targetMajorVersion+1, err)
	}
	return nil
}
