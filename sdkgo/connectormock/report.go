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

type callReport struct {
	Calls []reportedCall `json:"calls"`
}

const (
	reportLockRetry = 5 * time.Millisecond
	// reportLockStale is how long a lock file may exist before its holder is
	// presumed dead; one write takes milliseconds.
	reportLockStale = 10 * time.Second
)

// lockReport takes the report's cross-process lock, a file created
// exclusively, and returns the function that releases it. A lock file older
// than reportLockStale belonged to a killed process and is removed.
func lockReport(path string) (func(), error) {
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
		time.Sleep(reportLockRetry)
	}
}

// reportWriters serializes the report writes of the goroutines of this process;
// the file lock serializes those of several test processes.
var reportWriters sync.Mutex

// appendToReport adds call to the report file in the directory named by
// ReportDirectoryEnvironment, and does nothing when it is unset. The file is
// rewritten through a temporary file and a rename, so a reader never sees a
// partial document, and its read-modify-write holds a lock file, so parallel tests and several test processes never lose a
// call.
func appendToReport(call RecordedCall) error {
	directory := os.Getenv(ReportDirectoryEnvironment)
	if directory == "" {
		return nil
	}
	reportWriters.Lock()
	defer reportWriters.Unlock()
	path := filepath.Join(directory, ReportFileName)
	unlock, err := lockReport(path + ".lock")
	if err != nil {
		return fmt.Errorf("lock the connector call report: %w", err)
	}
	defer unlock()

	var report callReport
	switch existing, err := os.ReadFile(path); {
	case err == nil:
		if err := json.Unmarshal(existing, &report); err != nil {
			return fmt.Errorf("read the connector call report %s: %w", path, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("read the connector call report: %w", err)
	}
	report.Calls = append(report.Calls, reportedCall{
		Connection: call.ConnectionName, Operation: call.OperationID, Branch: call.Branch,
		ConnectorID: call.ConnectorID, FlowID: call.FlowID, CallID: call.CallID,
		IsRetry: call.IsRetry, IsReplay: call.IsReplay, IsUnscripted: call.IsUnscripted,
	})
	encoded, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode the connector call report: %w", err)
	}
	temporary := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return fmt.Errorf("write the connector call report: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("replace the connector call report: %w", err)
	}
	return nil
}
