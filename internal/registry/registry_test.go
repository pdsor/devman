package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devman-project/devman/internal/storage"
	"github.com/devman-project/devman/pkg/errs"
)

const projectYAML = `version: 1

project:
  name: sample-app

services:
  frontend:
    display_name: Web
    runtime: host
    cwd: ./frontend
    command: %COMMAND%
    args: [dev]
    ports:
      - name: http
        value: auto
        env: PORT

  worker:
    runtime: host
    cwd: ./worker
    command: %COMMAND%
    args: [work]
`

// hostCommand is an executable that exists on every platform, so validation's
// PATH check does not produce noise in these tests.
func hostCommand() string {
	if os.PathSeparator == '\\' {
		return "cmd.exe"
	}
	return "sh"
}

func newProjectDir(t *testing.T, yaml string) string {
	t.Helper()
	root := t.TempDir()
	for _, sub := range []string{"frontend", "worker"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(t, root, yaml)
	return root
}

func writeConfig(t *testing.T, root, yaml string) {
	t.Helper()
	body := strings.ReplaceAll(yaml, "%COMMAND%", hostCommand())
	if err := os.WriteFile(filepath.Join(root, "devman.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newRegistry(t *testing.T) *Registry {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "devman.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db)
}

func TestInspectDescribesWhatWillRun(t *testing.T) {
	reg := newRegistry(t)
	root := newProjectDir(t, projectYAML)

	preview, err := reg.Inspect(root)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if preview.Name != "sample-app" || preview.Path != root {
		t.Fatalf("preview = %+v", preview)
	}
	if !preview.Validation.Valid {
		t.Fatalf("validation errors: %+v", preview.Validation.Errors)
	}
	if preview.AlreadyRegistered {
		t.Fatal("a fresh directory must not be reported as registered")
	}
	if !preview.TrustRequired {
		t.Fatal("a new project must require trust")
	}
	if len(preview.Execution) != 2 {
		t.Fatalf("execution summary = %+v", preview.Execution)
	}
	if !strings.Contains(preview.Execution[0].CommandLine, "dev") {
		t.Fatalf("summary must show the command line: %+v", preview.Execution[0])
	}
	if preview.Fingerprint == "" {
		t.Fatal("fingerprint missing")
	}
}

func TestInspectRejectsMissingConfig(t *testing.T) {
	reg := newRegistry(t)
	if _, err := reg.Inspect(t.TempDir()); !errs.Is(err, errs.CodeConfigNotFound) {
		t.Fatalf("err = %v, want CONFIG_NOT_FOUND", err)
	}
}

func TestRegisterRefusesInvalidConfig(t *testing.T) {
	reg := newRegistry(t)
	root := newProjectDir(t, `version: 1
project:
  name: broken
services:
  api:
    command: %COMMAND%
    cwd: ./does-not-exist
`)
	if _, err := reg.Register(root, true); !errs.Is(err, errs.CodeConfigInvalid) {
		t.Fatalf("err = %v, want CONFIG_INVALID", err)
	}
	if _, err := reg.Projects(); err != nil {
		t.Fatal(err)
	} else if projects, _ := reg.Projects(); len(projects) != 0 {
		t.Fatalf("an invalid project must not be registered: %+v", projects)
	}
}

func TestRegisterWithoutTrustBlocksStart(t *testing.T) {
	reg := newRegistry(t)
	root := newProjectDir(t, projectYAML)

	project, err := reg.Register(root, false)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if project.TrustedFingerprint != "" {
		t.Fatal("registering without trust must not record a fingerprint")
	}
	err = reg.EnsureTrusted(project.ID)
	if !errs.Is(err, errs.CodeProjectUntrusted) {
		t.Fatalf("EnsureTrusted = %v, want PROJECT_UNTRUSTED", err)
	}

	if err := reg.Trust(project.ID); err != nil {
		t.Fatalf("Trust: %v", err)
	}
	if err := reg.EnsureTrusted(project.ID); err != nil {
		t.Fatalf("EnsureTrusted after Trust: %v", err)
	}
}

func TestCosmeticEditKeepsTrustButCommandChangeRevokesIt(t *testing.T) {
	reg := newRegistry(t)
	root := newProjectDir(t, projectYAML)
	project, err := reg.Register(root, true)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.EnsureTrusted(project.ID); err != nil {
		t.Fatalf("freshly trusted project: %v", err)
	}

	// A display name change has no security meaning and must not force the user
	// to approve the project again.
	writeConfig(t, root, strings.Replace(projectYAML, "display_name: Web", "display_name: Frontend UI", 1))
	touch(t, filepath.Join(root, "devman.yaml"))
	if err := reg.EnsureTrusted(project.ID); err != nil {
		t.Fatalf("cosmetic edit revoked trust: %v", err)
	}

	// Changing what actually runs must revoke trust.
	writeConfig(t, root, strings.Replace(projectYAML, "args: [dev]", "args: [/c, whoami]", 1))
	touch(t, filepath.Join(root, "devman.yaml"))
	if err := reg.EnsureTrusted(project.ID); !errs.Is(err, errs.CodeProjectUntrusted) {
		t.Fatalf("changing args must revoke trust, got %v", err)
	}
	details := errs.From(reg.EnsureTrusted(project.ID)).Details
	if details["current_fingerprint"] == details["trusted_fingerprint"] {
		t.Fatalf("error should expose both fingerprints: %+v", details)
	}

	// Re-registering with trust re-approves the new fingerprint.
	if _, err := reg.Register(root, true); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if err := reg.EnsureTrusted(project.ID); err != nil {
		t.Fatalf("re-trust failed: %v", err)
	}
}

func TestConfigCacheReloadsOnChange(t *testing.T) {
	reg := newRegistry(t)
	root := newProjectDir(t, projectYAML)
	project, err := reg.Register(root, true)
	if err != nil {
		t.Fatal(err)
	}

	first, err := reg.Config(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := reg.Config(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatal("an unchanged file must be served from cache")
	}

	writeConfig(t, root, strings.Replace(projectYAML, "name: sample-app", "name: renamed-app", 1))
	touch(t, filepath.Join(root, "devman.yaml"))

	reloaded, err := reg.Config(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Project.Name != "renamed-app" {
		t.Fatalf("edit not picked up: %q", reloaded.Project.Name)
	}
}

func TestValidConfigReportsLaterBreakage(t *testing.T) {
	reg := newRegistry(t)
	root := newProjectDir(t, projectYAML)
	project, err := reg.Register(root, true)
	if err != nil {
		t.Fatal(err)
	}

	// Break the config after a successful registration.
	writeConfig(t, root, strings.Replace(projectYAML, "cwd: ./frontend", "cwd: ./gone", 1))
	touch(t, filepath.Join(root, "devman.yaml"))

	if _, err := reg.ValidConfig(project.ID); !errs.Is(err, errs.CodeConfigInvalid) {
		t.Fatalf("ValidConfig = %v, want CONFIG_INVALID", err)
	}
	// The raw config is still available so the GUI can show the error in context.
	if _, validation, err := reg.ConfigWithValidation(project.ID); err != nil || validation.Valid {
		t.Fatalf("expected an invalid validation result, got %v, %v", validation, err)
	}
}

func TestResolveSelectors(t *testing.T) {
	reg := newRegistry(t)
	root := newProjectDir(t, projectYAML)
	project, err := reg.Register(root, true)
	if err != nil {
		t.Fatal(err)
	}

	for name, selector := range map[string]string{
		"by id":   project.ID,
		"by name": "sample-app",
		"by path": root,
	} {
		found, err := reg.Resolve(selector, os.TempDir())
		if err != nil || found.ID != project.ID {
			t.Fatalf("%s: %+v, %v", name, found, err)
		}
	}

	// An empty selector resolves the project containing the working directory,
	// including from a subdirectory.
	found, err := reg.Resolve("", filepath.Join(root, "frontend"))
	if err != nil || found.ID != project.ID {
		t.Fatalf("cwd resolution: %+v, %v", found, err)
	}

	if _, err := reg.Resolve("nope", os.TempDir()); !errs.Is(err, errs.CodeProjectNotFound) {
		t.Fatalf("unknown selector err = %v", err)
	}
	if _, err := reg.Resolve("", os.TempDir()); !errs.Is(err, errs.CodeProjectNotFound) {
		t.Fatalf("outside any project err = %v", err)
	}
}

func TestUnregister(t *testing.T) {
	reg := newRegistry(t)
	root := newProjectDir(t, projectYAML)
	project, err := reg.Register(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if projects, err := reg.Projects(); err != nil || len(projects) != 1 {
		t.Fatalf("projects = %+v, %v", projects, err)
	}
	if err := reg.Unregister(project.ID); err != nil {
		t.Fatal(err)
	}
	if projects, err := reg.Projects(); err != nil || len(projects) != 0 {
		t.Fatalf("projects after unregister = %+v, %v", projects, err)
	}
	if _, err := reg.Config(project.ID); !errs.Is(err, errs.CodeProjectNotFound) {
		t.Fatalf("config after unregister = %v", err)
	}
}

func TestRegisterTwiceKeepsRegistrationTime(t *testing.T) {
	reg := newRegistry(t)
	root := newProjectDir(t, projectYAML)

	first, err := reg.Register(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.SetFavorite(first.ID, true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)

	second, err := reg.Register(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("created_at changed: %v -> %v", first.CreatedAt, second.CreatedAt)
	}
	if !second.Favorite {
		t.Fatal("favourite must survive re-registration")
	}
	// Re-registering without --trust must not silently drop existing trust.
	if second.TrustedFingerprint == "" {
		t.Fatal("existing trust must be preserved when re-registering without --trust")
	}
}

// touch forces a new modification time even on filesystems with coarse
// timestamps, so the config cache reliably notices the change.
func touch(t *testing.T, path string) {
	t.Helper()
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
}
