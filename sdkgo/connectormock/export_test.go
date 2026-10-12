// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package connectormock

import "time"

// SetReportLockLimitsForTest shortens the report lock's timeout and stale age
// and returns the function that restores them.
func SetReportLockLimitsForTest(timeout, stale time.Duration) func() {
	previousTimeout, previousStale := reportLockTimeout, reportLockStale
	reportLockTimeout, reportLockStale = timeout, stale
	return func() { reportLockTimeout, reportLockStale = previousTimeout, previousStale }
}
