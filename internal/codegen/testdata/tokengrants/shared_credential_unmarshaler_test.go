// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package tokengrants_test

import (
	"example.com/tokengrants"
	"github.com/superdurable/dex-connectors-library/sdkgo/projectconfig"
)

// The Connector SDK decodes a shared entry with the generated method only when it satisfies this interface.
var _ projectconfig.SharedCredentialUnmarshaler = (*tokengrants.Credentials)(nil)
