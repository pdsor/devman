//go:build integration

// Package host holds the cross-platform runtime suite.
//
// These tests exist because "it compiles on three operating systems" is not the
// same claim as "it manages processes correctly on three operating systems".
// Everything here is about behaviour the operating system owns and DevMan can
// only ask for: terminating a whole process tree, handing out ports under
// concurrency, and telling the truth after the daemon itself dies.
//
// The suite needs no Docker and no external runtime, so it is a blocking gate on
// windows-latest, ubuntu-latest and macos-latest alike. It sits behind the
// `integration` build tag only because it is slower than a unit test and is run
// as its own CI job.
package host

import (
	"testing"

	"github.com/devman-project/devman/internal/testenv"
)

// This package's private daemon port window. Every suite gets its own because
// `go test ./...` runs packages in parallel, and because a suite must never bind
// the port a developer's real daemon is on.
var window = testenv.PortWindow{Start: 39800, End: 39849}

// TestMain also serves the fixture: with DEVMAN_TEST_HELPER set, this binary is
// the service under supervision, so the suite needs no node or python.
func TestMain(m *testing.M) { testenv.RunMain(m) }

func newStack(t *testing.T) *testenv.Stack {
	return testenv.NewStack(t, testenv.NewLayout(t), window)
}

// The tests read better with unqualified names.
var (
	writeProject  = testenv.WriteProject
	waitFor       = testenv.WaitFor
	singleService = testenv.SingleService
	free          = testenv.Free
)
