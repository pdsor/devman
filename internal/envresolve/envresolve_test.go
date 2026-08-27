package envresolve

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrecedenceRunsFromDaemonEnvToInjection(t *testing.T) {
	layers := Layers{
		Base:      map[string]string{"SHARED": "daemon", "PATH": "/usr/bin"},
		Files:     []map[string]string{{"SHARED": "file1", "ONLY_FILE": "yes"}, {"SHARED": "file2"}},
		Service:   map[string]string{"SHARED": "service"},
		Injection: map[string]string{"SHARED": "injected", "PORT": "8123"},
	}

	final := layers.Final()
	if final["SHARED"] != "injected" {
		t.Fatalf("injection must win, got %q", final["SHARED"])
	}
	if final["ONLY_FILE"] != "yes" {
		t.Fatal("env_file entries must survive")
	}

	// The later env_file wins over the earlier one: files are applied in
	// declaration order, which is what makes `.env` then `.env.local` work.
	withoutService := Layers{Base: layers.Base, Files: layers.Files}.UserEnv()
	if withoutService["SHARED"] != "file2" {
		t.Fatalf("the last env_file must win, got %q", withoutService["SHARED"])
	}

	// ${ENV:...} sees the user layers only, so a template can never depend on a
	// port that has not been allocated yet.
	user := layers.UserEnv()
	if user["SHARED"] != "service" {
		t.Fatalf("service env must win over env_file, got %q", user["SHARED"])
	}
	if _, visible := user["PORT"]; visible {
		t.Fatal("DevMan injection must not be visible to ${ENV:...}")
	}
}

func TestLoadFileParsesDotenvForms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	body := "# comment\n" +
		"export EXPORTED=1\n" +
		"PLAIN=value\n" +
		"QUOTED=\"line\\nbreak\"\n" +
		"SINGLE='raw \\n text'\n" +
		"TRAILING=value # inline comment\n" +
		"EMPTY=\n" +
		"malformed\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	parsed, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{
		"EXPORTED": "1",
		"PLAIN":    "value",
		"QUOTED":   "line\nbreak",
		"SINGLE":   `raw \n text`,
		"TRAILING": "value",
		"EMPTY":    "",
	}
	for key, want := range expected {
		if parsed[key] != want {
			t.Fatalf("%s: expected %q, got %q", key, want, parsed[key])
		}
	}
	if _, ok := parsed["malformed"]; ok {
		t.Fatal("a line without = must be ignored, not guessed at")
	}
}

func TestMissingEnvFileIsNotAnError(t *testing.T) {
	// `.env.local` being absent is normal; validation warns, starting does not
	// fail.
	parsed, err := LoadFile(filepath.Join(t.TempDir(), "nope.env"))
	if err != nil {
		t.Fatalf("a missing env file must not fail: %v", err)
	}
	if len(parsed) != 0 {
		t.Fatal("expected an empty result")
	}
}

func TestMissingRequiredDetectsEmptyValues(t *testing.T) {
	env := map[string]string{"SET": "value", "BLANK": "   "}
	missing := MissingRequired([]string{"SET", "BLANK", "ABSENT"}, env)
	if len(missing) != 2 || missing[0] != "BLANK" || missing[1] != "ABSENT" {
		t.Fatalf("expected BLANK and ABSENT to be missing, got %v", missing)
	}
}

func TestLookupRejectsUnknownCommandWithDevManError(t *testing.T) {
	resolver := Resolver{}
	if _, err := resolver.Lookup("devman-definitely-missing-xyz", t.TempDir(), os.Getenv("PATH")); err == nil {
		t.Fatal("expected a COMMAND_NOT_FOUND error")
	}
}

func TestApplyPathPrependsAdditionalDirectories(t *testing.T) {
	resolver := Resolver{AdditionalPath: []string{"/opt/tools"}, ExtraEnv: map[string]string{"EXTRA": "1"}}
	env := map[string]string{"PATH": "/usr/bin"}
	resolver.ApplyPath(env)

	if env["PATH"] == "/usr/bin" {
		t.Fatal("additional_path must be prepended to PATH")
	}
	if env["EXTRA"] != "1" {
		t.Fatal("global extra env must be applied")
	}

	// An explicit value always beats the global default.
	env2 := map[string]string{"PATH": "/usr/bin", "EXTRA": "project"}
	resolver.ApplyPath(env2)
	if env2["EXTRA"] != "project" {
		t.Fatalf("global env must not override an explicit value, got %q", env2["EXTRA"])
	}
}

func TestEnvironIsSortedForDeterministicSpawns(t *testing.T) {
	entries := Environ(map[string]string{"B": "2", "A": "1", "C": "3"})
	if len(entries) != 3 || entries[0] != "A=1" || entries[1] != "B=2" || entries[2] != "C=3" {
		t.Fatalf("expected a sorted environment, got %v", entries)
	}
}
