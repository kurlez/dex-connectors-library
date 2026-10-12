// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConnectorREADMEsNameTheManifestVersion keeps every version that a connector README pins for its own module on
// the manifest version: the go get commands and the raw tag URLs of its examples, the release-artifact command's
// --version and --tag, and the "Connector `vX.Y.Z`" and "Before `vX.Y.Z` is published" sentences. A version of
// another module, the SDK, or Dex is not matched.
func TestConnectorREADMEsNameTheManifestVersion(t *testing.T) {
	const version = `v\d+\.\d+\.\d+`
	for _, directory := range registeredConnectorDirectories(t) {
		root := filepath.Join("..", "..", filepath.FromSlash(directory))
		manifestVersion := readManifestIdentity(t, directory).Version
		module := regexp.QuoteMeta(directory)
		patterns := []*regexp.Regexp{
			regexp.MustCompile(module + `(?:/[A-Za-z0-9._-]+)*?[@/](` + version + `)\b`),
			regexp.MustCompile("--version (" + version + `)\b`),
			regexp.MustCompile("Connector `(" + version + ")`"),
			regexp.MustCompile("Before `(" + version + ")` is published"),
		}
		require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			require.NoError(t, err)
			if entry.IsDir() && entry.Name() == "node_modules" {
				return filepath.SkipDir
			}
			if entry.IsDir() || entry.Name() != "README.md" {
				return nil
			}
			contents, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			for _, pattern := range patterns {
				for _, match := range pattern.FindAllStringSubmatch(string(contents), -1) {
					require.Equal(t, manifestVersion, match[1], "%s pins %q", strings.TrimPrefix(path, "../../"), match[0])
				}
			}
			return nil
		}))
	}
}
