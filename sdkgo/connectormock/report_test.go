// Copyright (c) 2026 Super Durable
// SPDX-License-Identifier: MIT

package connectormock_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/superdurable/dex-connectors-library/sdkgo"
	"github.com/superdurable/dex-connectors-library/sdkgo/connectormock"
	"github.com/superdurable/dex-connectors-library/sdkgo/internal/testsupport"
)

const reportChildCalls = 5

type reportDocument struct {
	Calls []map[string]any `json:"calls"`
}

func readReport(t *testing.T, directory string) reportDocument {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join(directory, connectormock.ReportFileName))
	require.NoError(t, err)
	var document reportDocument
	require.NoError(t, json.Unmarshal(encoded, &document))
	return document
}

func callIDOf(t *testing.T, lookup *connectormock.Query[widgetInput, widget], index int) string {
	t.Helper()
	return string(lookup.Calls()[index].CallID)
}

func answerLookups(t *testing.T, flowID string, count int) {
	lookup := connectormock.NewQuery[widgetInput, widget](t, lookupDefinition)
	lookup.Default(connectormock.Branch(widgetFound, widget{ID: "gear"}, nil))
	for step := range count {
		runQuery(t, lookup, flowID, step, widgetInput{Name: "gear"})
	}
}

func TestReportIsWrittenOnlyWhenTheDirectoryIsSet(t *testing.T) {
	directory := t.TempDir()
	t.Setenv(connectormock.ReportDirectoryEnvironment, "")

	answerLookups(t, uniqueFlowID(t), 2)

	require.NoFileExists(t, filepath.Join(directory, connectormock.ReportFileName))
}

func TestReportRecordsConnectionOperationAndBranch(t *testing.T) {
	directory := t.TempDir()
	t.Setenv(connectormock.ReportDirectoryEnvironment, directory)
	flowID := uniqueFlowID(t)

	lookup := connectormock.NewQuery[widgetInput, widget](t, lookupDefinition)
	lookup.Respond(connectormock.Branch(widgetAbsent, widget{}, nil), connectormock.Retry[widget](sdkgo.Failure{Kind: sdkgo.FailureAvailability, Message: "busy"}, 0))
	runQuery(t, lookup, flowID, 0, widgetInput{Name: "gear"})
	_, err := sdkgo.RunQuery(testsupport.NewDexContext(flowID, "step-1"), lookup, widgetConnection, widgetInput{Name: "gear"})
	require.Error(t, err)

	require.Equal(t, reportDocument{Calls: []map[string]any{
		{"connection": "primary", "operation": "lookupWidget", "branch": "absent", "connectorId": "widgets", "flowId": flowID, "callId": callIDOf(t, lookup, 0)},
		{"connection": "primary", "operation": "lookupWidget", "connectorId": "widgets", "flowId": flowID, "callId": callIDOf(t, lookup, 1), "retry": true},
	}}, readReport(t, directory))
}

func TestReportKeepsEveryCallOfParallelTestsAndProcesses(t *testing.T) {
	directory := t.TempDir()
	t.Setenv(connectormock.ReportDirectoryEnvironment, directory)
	const goroutines, goroutineCalls, children = 8, 6, 3

	var group sync.WaitGroup
	started := make([]*exec.Cmd, children)
	for index := range children {
		command := exec.Command(os.Args[0], "-test.run", "^TestReportChildProcess$")
		command.Env = append(os.Environ(), "CONNECTORMOCK_REPORT_CHILD=child-"+fmt.Sprint(index))
		require.NoError(t, command.Start())
		started[index] = command
	}
	for index := range goroutines {
		group.Add(1)
		go func() {
			defer group.Done()
			answerLookups(t, fmt.Sprintf("parallel-%d", index), goroutineCalls)
		}()
	}
	group.Wait()
	for _, command := range started {
		require.NoError(t, command.Wait())
	}

	document := readReport(t, directory)
	require.Len(t, document.Calls, goroutines*goroutineCalls+children*reportChildCalls)
	require.NoFileExists(t, filepath.Join(directory, connectormock.ReportFileName+".tmp"))
}

// TestReportChildProcess is the body of the processes that
// TestReportKeepsEveryCallOfParallelTestsAndProcesses starts; alone it passes
// without doing anything.
func TestReportChildProcess(t *testing.T) {
	flowID := os.Getenv("CONNECTORMOCK_REPORT_CHILD")
	if flowID == "" {
		t.Skip("runs only as a child of the report test")
	}
	answerLookups(t, flowID, reportChildCalls)
}

func TestReportWriteFailureFailsTheTest(t *testing.T) {
	t.Setenv(connectormock.ReportDirectoryEnvironment, filepath.Join(t.TempDir(), "missing"))
	recorder := newRecordingT(t)
	lookup := connectormock.NewQuery[widgetInput, widget](recorder, lookupDefinition)
	lookup.Default(connectormock.Branch(widgetFound, widget{ID: "gear"}, nil))

	runQuery(t, lookup, uniqueFlowID(t), 0, widgetInput{Name: "gear"})

	recorder.requireFailure(t, "lock the connector call report")
}

func TestReportLockHeldTooLongFailsTheCall(t *testing.T) {
	directory := t.TempDir()
	t.Setenv(connectormock.ReportDirectoryEnvironment, directory)
	t.Cleanup(connectormock.SetReportLockLimitsForTest(100*time.Millisecond, time.Hour))
	require.NoError(t, os.WriteFile(filepath.Join(directory, connectormock.ReportFileName+".lock"), nil, 0o600))
	recorder := newRecordingT(t)
	lookup := connectormock.NewQuery[widgetInput, widget](recorder, lookupDefinition)
	lookup.Default(connectormock.Branch(widgetFound, widget{ID: "gear"}, nil))

	runQuery(t, lookup, uniqueFlowID(t), 0, widgetInput{Name: "gear"})

	recorder.requireFailure(t, "stayed held for 100ms")
}

func TestReportStaleLockIsBroken(t *testing.T) {
	directory := t.TempDir()
	t.Setenv(connectormock.ReportDirectoryEnvironment, directory)
	t.Cleanup(connectormock.SetReportLockLimitsForTest(time.Minute, time.Nanosecond))
	require.NoError(t, os.WriteFile(filepath.Join(directory, connectormock.ReportFileName+".lock"), nil, 0o600))

	answerLookups(t, uniqueFlowID(t), 1)

	require.Len(t, readReport(t, directory).Calls, 1)
}

func TestReportThatDoesNotEndAsADocumentFailsTheCall(t *testing.T) {
	directory := t.TempDir()
	t.Setenv(connectormock.ReportDirectoryEnvironment, directory)
	require.NoError(t, os.WriteFile(filepath.Join(directory, connectormock.ReportFileName), []byte("garbage"), 0o600))
	recorder := newRecordingT(t)
	lookup := connectormock.NewQuery[widgetInput, widget](recorder, lookupDefinition)
	lookup.Default(connectormock.Branch(widgetFound, widget{ID: "gear"}, nil))

	runQuery(t, lookup, uniqueFlowID(t), 0, widgetInput{Name: "gear"})

	recorder.requireFailure(t, "does not end with")
}
