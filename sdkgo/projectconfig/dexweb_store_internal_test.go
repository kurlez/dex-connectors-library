// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package projectconfig

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVersionedStoreKeepsHistoryOnlyForConfigurationSnapshot(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	store, err := newVersionedStore(directory)
	require.NoError(t, err)
	const snapshotKey, headKey = "projects/local/preview/configuration/head", "projects/local/preview/connections/x/y/head"
	for _, key := range []string{snapshotKey, headKey} {
		object, err := store.CreateObject(ctx, key, []byte("v1"))
		require.NoError(t, err)
		for _, contents := range []string{"v2", "v3"} {
			object, err = store.CompareAndSwapObject(ctx, key, object.ETag, []byte(contents))
			require.NoError(t, err)
		}
		require.Equal(t, "3", object.Version)
	}

	pinned, err := store.ReadObject(ctx, snapshotKey, "1")
	require.NoError(t, err)
	require.Equal(t, "v1", string(pinned.Contents))
	_, err = store.ReadObject(ctx, headKey, "1")
	require.ErrorIs(t, err, ErrObjectNotFound)
	current, err := store.ReadObject(ctx, headKey, "")
	require.NoError(t, err)
	require.Equal(t, "v3", string(current.Contents))

	files, err := filepath.Glob(filepath.Join(directory, "*.json"))
	require.NoError(t, err)
	require.Len(t, files, 4) // three snapshot versions and one head

	// A crash after writing a new version leaves the old file; loading keeps only the newest.
	stale, err := os.ReadFile(files[0])
	require.NoError(t, err)
	reloaded, err := newVersionedStore(directory)
	require.NoError(t, err)
	require.NotEmpty(t, stale)
	current, err = reloaded.ReadObject(ctx, headKey, "")
	require.NoError(t, err)
	require.Equal(t, "v3", string(current.Contents))
}
