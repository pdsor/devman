package platform

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as its own fixture: re-executing os.Args[0] with
// DEVMAN_TEST_HELPER set gives us tiny, dependency-free processes that behave
// like real dev servers (spawning children, ignoring signals, exiting with a
// code) on all three platforms.
const helperEnv = "DEVMAN_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "echo":
		fmt.Fprintln(os.Stdout, "hello stdout")
		fmt.Fprintln(os.Stderr, "hello stderr")
		os.Exit(3)
	case "graceful":
		// Exits cleanly on SIGTERM (Unix) or CTRL_BREAK (Windows, which the Go
		// runtime delivers as os.Interrupt).
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		fmt.Fprintln(os.Stdout, "ready")
		select {
		case <-ch:
			os.Exit(7)
		case <-time.After(60 * time.Second):
			os.Exit(99)
		}
	case "stubborn":
		// Captures and discards shutdown signals: only a force kill stops it.
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		fmt.Fprintln(os.Stdout, "ready")
		go func() {
			for range ch {
			}
		}()
		time.Sleep(120 * time.Second)
		os.Exit(98)
	case "tree":
		// Spawns a grandchild, then refuses to die. Stopping the service must
		// take the grandchild down too.
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), helperEnv+"=stubborn")
		child.Stdout = nil
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "cannot spawn grandchild:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "grandchild %d\n", child.Process.Pid)
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		go func() {
			for range ch {
			}
		}()
		time.Sleep(120 * time.Second)
		os.Exit(97)
	default:
		fmt.Fprintln(os.Stderr, "unknown helper mode")
		os.Exit(2)
	}
}

func helperRequest(mode string, stdout, stderr *syncBuffer) SpawnRequest {
	return SpawnRequest{
		Command: os.Args[0],
		Dir:     os.TempDir(),
		Env:     append(os.Environ(), helperEnv+"="+mode),
		Stdout:  stdout,
		Stderr:  stderr,
	}
}

// syncBuffer is a concurrency-safe sink for captured output.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSpawnCapturesStreamsAndExitCode(t *testing.T) {
	var stdout, stderr syncBuffer
	proc, err := Spawn(helperRequest("echo", &stdout, &stderr))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	status := proc.Wait()

	if status.Code != 3 {
		t.Fatalf("exit code = %d, want 3", status.Code)
	}
	if status.ExitedAt.IsZero() {
		t.Fatal("exited_at not recorded")
	}
	if got := stdout.String(); !strings.Contains(got, "hello stdout") {
		t.Fatalf("stdout = %q", got)
	}
	if got := stderr.String(); !strings.Contains(got, "hello stderr") {
		t.Fatalf("stderr = %q", got)
	}
	if strings.Contains(stdout.String(), "stderr") {
		t.Fatal("streams must not be merged")
	}
}

func TestIdentityIsRecordedAtSpawn(t *testing.T) {
	var stdout, stderr syncBuffer
	proc, err := Spawn(helperRequest("graceful", &stdout, &stderr))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer proc.Stop(time.Second)

	id := proc.Identity()
	if id.PID != proc.PID() || id.PID <= 0 {
		t.Fatalf("identity pid = %d", id.PID)
	}
	if id.SpawnedAt.IsZero() || id.Executable == "" || len(id.Fingerprint) != 32 {
		t.Fatalf("incomplete identity: %+v", id)
	}

	info, err := Inspect(id.PID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !info.MatchesIdentity(id) {
		t.Fatalf("live process must match its own identity: info=%+v id=%+v", info, id)
	}

	// A recycled PID running something else must not match.
	stale := id
	stale.SpawnedAt = id.SpawnedAt.Add(-2 * time.Hour)
	if info.MatchesIdentity(stale) {
		t.Fatal("start time skew must be rejected")
	}
	wrongExe := id
	wrongExe.Executable = "/usr/bin/definitely-not-this"
	if info.MatchesIdentity(wrongExe) {
		t.Fatal("executable mismatch must be rejected")
	}
}

func TestCommandFingerprintDistinguishesInvocations(t *testing.T) {
	base := CommandFingerprint("pnpm", []string{"dev"}, "/proj")
	if base == CommandFingerprint("pnpm", []string{"start"}, "/proj") {
		t.Fatal("args must affect the fingerprint")
	}
	if base == CommandFingerprint("pnpm", []string{"dev"}, "/other") {
		t.Fatal("cwd must affect the fingerprint")
	}
	if base != CommandFingerprint("pnpm", []string{"dev"}, "/proj") {
		t.Fatal("fingerprint must be deterministic")
	}
}

func TestStopGracefulSignal(t *testing.T) {
	// This mirrors what the daemon does at startup on Windows: without a
	// console, CTRL_BREAK cannot reach a child process group at all.
	if err := EnsureConsole(); err != nil {
		t.Skipf("cannot attach a console: %v", err)
	}
	if !HasConsole() {
		t.Skip("no console: graceful signalling is not available in this environment")
	}
	var stdout, stderr syncBuffer
	proc, err := Spawn(helperRequest("graceful", &stdout, &stderr))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitFor(t, 10*time.Second, "helper readiness", func() bool {
		return strings.Contains(stdout.String(), "ready")
	})

	outcome := proc.Stop(10 * time.Second)
	if !outcome.SignalSent {
		t.Fatalf("graceful signal was not delivered: %s", outcome.SignalError)
	}
	if !outcome.Graceful {
		t.Fatalf("expected a graceful exit, got %+v", outcome)
	}
	if outcome.Status.Code != 7 {
		t.Fatalf("exit code = %d, want 7 (the helper's clean shutdown code)", outcome.Status.Code)
	}
	if outcome.Status.Killed {
		t.Fatal("a graceful exit must not be reported as killed")
	}
}

func TestStopForceKillsProcessThatIgnoresSignals(t *testing.T) {
	var stdout, stderr syncBuffer
	proc, err := Spawn(helperRequest("stubborn", &stdout, &stderr))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitFor(t, 10*time.Second, "helper readiness", func() bool {
		return strings.Contains(stdout.String(), "ready")
	})
	pid := proc.PID()

	outcome := proc.Stop(500 * time.Millisecond)
	if outcome.Graceful {
		t.Fatal("a process that ignores the signal must not report a graceful exit")
	}
	if !outcome.Status.Killed {
		t.Fatalf("expected killed=true, got %+v", outcome.Status)
	}
	waitFor(t, 10*time.Second, "process to disappear", func() bool { return !Alive(pid) })
}

func TestStopTerminatesWholeProcessTree(t *testing.T) {
	var stdout, stderr syncBuffer
	proc, err := Spawn(helperRequest("tree", &stdout, &stderr))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	var grandchild int
	waitFor(t, 15*time.Second, "grandchild pid", func() bool {
		for _, line := range strings.Split(stdout.String(), "\n") {
			if strings.HasPrefix(line, "grandchild ") {
				pid, convErr := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "grandchild ")))
				if convErr == nil {
					grandchild = pid
					return true
				}
			}
		}
		return false
	})
	if !Alive(grandchild) {
		t.Fatalf("grandchild %d should be running", grandchild)
	}
	parent := proc.PID()

	proc.Stop(500 * time.Millisecond)

	waitFor(t, 15*time.Second, "parent to exit", func() bool { return !Alive(parent) })
	// This is the requirement that rules out "just kill the parent PID".
	waitFor(t, 15*time.Second, "grandchild to exit", func() bool { return !Alive(grandchild) })
}

func TestStopIsIdempotent(t *testing.T) {
	var stdout, stderr syncBuffer
	proc, err := Spawn(helperRequest("echo", &stdout, &stderr))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	proc.Wait()

	first := proc.Stop(time.Second)
	second := proc.Stop(time.Second)
	if first.Status.Code != 3 || second.Status.Code != 3 {
		t.Fatalf("stopping an exited process must report its real status: %+v %+v", first, second)
	}
	if proc.Running() {
		t.Fatal("Running must be false after exit")
	}
}

func TestSpawnRejectsEmptyCommand(t *testing.T) {
	if _, err := Spawn(SpawnRequest{}); err == nil {
		t.Fatal("expected an error for an empty command")
	}
}

func TestSpawnRejectsMissingExecutable(t *testing.T) {
	_, err := Spawn(SpawnRequest{Command: "devman-definitely-not-a-real-binary"})
	if err == nil {
		t.Fatal("expected an error for a missing executable")
	}
}

func TestInspectRejectsDeadPID(t *testing.T) {
	var stdout, stderr syncBuffer
	proc, err := Spawn(helperRequest("echo", &stdout, &stderr))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	pid := proc.PID()
	proc.Wait()
	waitFor(t, 5*time.Second, "process to disappear", func() bool { return !Alive(pid) })
	if _, err := Inspect(pid); err == nil {
		t.Fatal("Inspect must fail for a dead PID")
	}
}
