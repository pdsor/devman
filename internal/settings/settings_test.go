package settings

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultsAreValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("default settings invalid: %v", err)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Daemon.PortStart != 39100 || s.Daemon.PortEnd != 39149 {
		t.Fatalf("daemon range = %d-%d", s.Daemon.PortStart, s.Daemon.PortEnd)
	}
	if r := s.Range("frontend"); r.Start != 3000 || r.End != 3999 {
		t.Fatalf("frontend range = %+v", r)
	}
	if r := s.Range("does-not-exist"); r.Start != 10000 {
		t.Fatalf("unknown range must fall back to general, got %+v", r)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := Default()
	original.Logs.MaxSizeMB = 25
	original.Defaults.GracefulTimeout.Duration = 15 * time.Second
	original.Environment.AdditionalPath = []string{"/opt/homebrew/bin"}
	if err := original.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Logs.MaxSizeMB != 25 {
		t.Fatalf("max_size_mb = %d", loaded.Logs.MaxSizeMB)
	}
	if loaded.Defaults.GracefulTimeout.Duration != 15*time.Second {
		t.Fatalf("graceful_timeout = %v", loaded.Defaults.GracefulTimeout.Duration)
	}
	if len(loaded.Environment.AdditionalPath) != 1 {
		t.Fatalf("additional_path = %v", loaded.Environment.AdditionalPath)
	}
}

func TestGetSet(t *testing.T) {
	s := Default()

	if got, err := s.Get("daemon.port_start"); err != nil || got != "39100" {
		t.Fatalf("get = %q, %v", got, err)
	}
	if err := s.Set("logs.max_backups", "7"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if s.Logs.MaxBackups != 7 {
		t.Fatalf("max_backups = %d", s.Logs.MaxBackups)
	}
	if err := s.Set("startup.daemon_on_login", "true"); err != nil {
		t.Fatalf("Set bool: %v", err)
	}
	if !s.Startup.DaemonOnLogin {
		t.Fatal("daemon_on_login not set")
	}
	if err := s.Set("port_ranges.worker.start", "9000"); err == nil {
		t.Fatal("creating a partial range must fail validation")
	}
	if err := s.Set("nope.nope", "1"); err == nil {
		t.Fatal("unknown key must fail")
	}
	// An invalid edit must not be persisted into the live struct.
	before := s.Daemon.Host
	if err := s.Set("daemon.host", "0.0.0.0"); err == nil {
		t.Fatal("binding to 0.0.0.0 must be rejected")
	}
	if s.Daemon.Host != before {
		t.Fatalf("rejected edit leaked: %q", s.Daemon.Host)
	}
}

func TestFlattenIncludesNestedRanges(t *testing.T) {
	flat := Default().Flatten()
	for _, key := range []string{
		"daemon.host",
		"daemon.port_start",
		"port_ranges.frontend.start",
		"logs.ring_buffer",
		"defaults.graceful_timeout",
		"startup.gui_on_login",
	} {
		if _, ok := flat[key]; !ok {
			t.Fatalf("missing key %q in %v", key, flat)
		}
	}
}
