// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package connectormock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/superdurable/dex-connectors-library/sdkgo"
)

const (
	// ReportDirectoryEnvironment names the environment variable that enables
	// the call report. When it holds a directory, every call that any mock in
	// the process answers is also written to ReportFileName in that directory.
	ReportDirectoryEnvironment = "DEX_CONNECTOR_MOCK_REPORT_DIR"
	// ReportFileName is the call report file inside the report directory.
	ReportFileName = "connector-calls.json"
)

// reportedCall is one entry of the call report. The JSON field names are the
// report's contract: a test harness reads connection, operation and branch to
// check that every wired branch was returned.
type reportedCall struct {
	Connection   string         `json:"connection"`
	Operation    string         `json:"operation"`
	Branch       sdkgo.BranchID `json:"branch,omitempty"`
	ConnectorID  string         `json:"connectorId"`
	FlowID       string         `json:"flowId,omitempty"`
	CallID       sdkgo.CallID   `json:"callId,omitempty"`
	IsRetry      bool           `json:"retry,omitempty"`
	IsReplay     bool           `json:"replay,omitempty"`
	IsUnscripted bool           `json:"unscripted,omitempty"`
}

// reportLockTimeout and reportLockStale are variables so a test can shorten
// them. A write takes milliseconds, so a lock held for reportLockStale belongs
// to a killed process, and a lock that stays held for reportLockTimeout fails
// the call that waits.
var (
	reportLockTimeout = 30 * time.Second
	reportLockStale   = 10 * time.Second
)

const reportLockRetry = 5 * time.Millisecond

// reportClose ends the document; a call is added by overwriting it.
const reportClose = "]}"

// lockReport takes the report's cross-process lock, a file created
// exclusively, and returns the function that releases it. A lock file older
// than reportLockStale belonged to a killed process and is removed. It fails
// when the lock stays held for reportLockTimeout.
func lockReport(path string) (func(), error) {
	deadline := time.Now().Add(reportLockTimeout)
	for {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = file.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > reportLockStale {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the lock %s stayed held for %s; remove it if no test process is running", path, reportLockTimeout)
		}
		time.Sleep(reportLockRetry)
	}
}

// reportWriters serializes the report writes of the goroutines of this process;
// the lock file serializes those of several test processes.
var reportWriters sync.Mutex

// appendToReport adds call to the report file in the directory named by
// ReportDirectoryEnvironment, and does nothing when it is unset. Under the
// lock it overwrites the document's closing "]}" with the new call and the
// closing again in one write, so the cost of a call does not grow with the
// calls before it, and a failed write fails the test.
func appendToReport(call RecordedCall) error {
	directory := os.Getenv(ReportDirectoryEnvironment)
	if directory == "" {
		return nil
	}
	encoded, err := json.Marshal(reportedCall{
		Connection: call.ConnectionName, Operation: call.OperationID, Branch: call.Branch,
		ConnectorID: call.ConnectorID, FlowID: call.FlowID, CallID: call.CallID,
		IsRetry: call.IsRetry, IsReplay: call.IsReplay, IsUnscripted: call.IsUnscripted,
	})
	if err != nil {
		return fmt.Errorf("encode the connector call report: %w", err)
	}
	reportWriters.Lock()
	defer reportWriters.Unlock()
	path := filepath.Join(directory, ReportFileName)
	unlock, err := lockReport(path + ".lock")
	if err != nil {
		return fmt.Errorf("lock the connector call report: %w", err)
	}
	defer unlock()

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open the connector call report: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat the connector call report: %w", err)
	}
	offset, entry := info.Size(), string(encoded)
	if offset == 0 {
		entry = `{"calls":[` + entry
	} else {
		offset -= int64(len(reportClose))
		tail := make([]byte, len(reportClose))
		if _, err := file.ReadAt(tail, offset); offset < 0 || err != nil || string(tail) != reportClose {
			return fmt.Errorf("the connector call report %s does not end with %q; remove it between runs", path, reportClose)
		}
		entry = "," + entry
	}
	if _, err := file.WriteAt([]byte(entry+reportClose), offset); err != nil {
		return fmt.Errorf("write the connector call report: %w", err)
	}
	return nil
}
