// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

// Package projectconfigtest loads project configurations in tests. Never import it from production code: it skips
// the storage-target checks that projectconfig.LoadFromEnvironment applies.
package projectconfigtest

import (
	"context"

	"github.com/superdurable/dex-connectors-library/sdkgo/internal/projectload"
	"github.com/superdurable/dex-connectors-library/sdkgo/projectconfig"
)

// LoadProject loads the configuration snapshot that reference pins from objects, as projectconfig.LoadFromEnvironment
// does after it builds its S3 store, and resolves credentials from the same store, a shared entry's through the
// document's credential broker. A test uses it with an in-memory store, such as one that serves a connector's shared
// entries and relayed Triggers from an in-process broker. It reads only the snapshot; a nil store is an error.
func LoadProject(ctx context.Context, objects projectconfig.ObjectStore, scope projectconfig.Scope, reference projectconfig.SnapshotRef) (*projectconfig.LoadedProject, error) {
	load := projectload.LoadFromObjectStore.(func(
		context.Context, projectconfig.ObjectStore, projectconfig.Scope, projectconfig.SnapshotRef,
	) (*projectconfig.LoadedProject, error))
	return load(ctx, objects, scope, reference)
}
