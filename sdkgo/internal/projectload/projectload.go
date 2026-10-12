// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

// Package projectload passes projectconfig's loader to its test-support package without a production export.
package projectload

// LoadFromObjectStore is projectconfig's
// func(context.Context, projectconfig.ObjectStore, projectconfig.Scope, projectconfig.SnapshotRef) (*projectconfig.LoadedProject, error),
// which loads a pinned snapshot from a caller's object store. projectconfig sets it when it initializes; it is untyped
// because this package cannot import projectconfig.
var LoadFromObjectStore any
