package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/devman-project/devman/pkg/errs"
)

const canonicalYAML = `
version: 1

project:
  name: my-ai-project

services:
  frontend:
    runtime: host
    cwd: ./frontend
    command: pnpm
    args:
      - dev
    ports:
      - name: http
        value: auto
        preferred: 3000
        env: PORT
    health:
      type: http
      url: http://127.0.0.1:${PORT}/
      interval: 5s
      timeout: 3s
      retries: 10

  backend:
    display_name: Backend API
    runtime: host
    cwd: ./backend
    command: uv
    args:
      - run
      - uvicorn
      - app.main:app
      - --port
      - ${PORT}
    env_file:
      - .env
    env:
      NODE_ENV: development
    required_env:
      - DATABASE_URL
    ports:
      - name: http
        value: auto
        env: PORT
      - name: debug
        value: auto
        env: DEBUG_PORT
    depends_on:
      - redis
    health:
      type: http
      url: http://127.0.0.1:${PORT:http}/health
    restart:
      policy: on-failure
      max_attempts: 3
      delay: 1s
      max_delay: 30s
    graceful_timeout: 20s

  redis:
    runtime: docker-compose
    compose:
      file: docker-compose.yml
      service: redis

  postgres:
    runtime: external
    health:
      type: tcp
      host: 127.0.0.1
      port: "5432"

startup:
  default:
    - frontend
    - backend

profiles:
  full:
    - frontend
    - backend
    - redis
`

func parseOK(t *testing.T, yamlText string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(yamlText))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

func TestParseCanonicalConfig(t *testing.T) {
	cfg := parseOK(t, canonicalYAML)

	if cfg.Version != 1 {
		t.Fatalf("version = %d", cfg.Version)
	}
	if got := cfg.ServiceNames(); len(got) != 4 || got[0] != "frontend" || got[3] != "postgres" {
		t.Fatalf("declaration order not preserved: %v", got)
	}

	frontend := cfg.Services["frontend"]
	if frontend.Name != "frontend" {
		t.Fatalf("service name not injected: %q", frontend.Name)
	}
	if frontend.Runtime != RuntimeHost {
		t.Fatalf("runtime = %q", frontend.Runtime)
	}
	if len(frontend.Ports) != 1 || !frontend.Ports[0].Value.Auto || frontend.Ports[0].Preferred != 3000 {
		t.Fatalf("unexpected ports: %+v", frontend.Ports)
	}
	if frontend.Ports[0].Range != "general" {
		t.Fatalf("port range default = %q, want general", frontend.Ports[0].Range)
	}

	backend := cfg.Services["backend"]
	if backend.Label() != "Backend API" {
		t.Fatalf("label = %q", backend.Label())
	}
	if backend.GracefulTimeout.Or(time.Second) != 20*time.Second {
		t.Fatalf("graceful_timeout = %v", backend.GracefulTimeout)
	}
	if backend.Restart.Policy != RestartOnFailure || backend.Restart.MaxAttempts != 3 {
		t.Fatalf("restart = %+v", backend.Restart)
	}
	if name, ok := backend.PrimaryPortName(); !ok || name != "http" {
		t.Fatalf("primary port = %q %v", name, ok)
	}

	// Absent health must default to process, never inferred from ports.
	if redis := cfg.Services["redis"]; redis.Health == nil || redis.Health.Type != HealthProcess {
		t.Fatalf("redis health = %+v, want process default", redis.Health)
	}
	// Absent restart must default to policy no.
	if redis := cfg.Services["redis"]; redis.Restart.Policy != RestartNo {
		t.Fatalf("redis restart = %+v", redis.Restart)
	}
}

func TestDependsOnBothFormsNormalise(t *testing.T) {
	listForm := parseOK(t, `
version: 1
project: {name: p}
services:
  a: {command: x, depends_on: [b, c]}
  b: {command: x}
  c: {command: x}
`)
	mapForm := parseOK(t, `
version: 1
project: {name: p}
services:
  a:
    command: x
    depends_on:
      b:
        condition: started
      c: {condition: healthy}
  b: {command: x}
  c:
    command: x
    health: {type: tcp, port: "1"}
`)

	if got := listForm.Services["a"].DependsOn; len(got) != 2 ||
		got[0] != (Dependency{Name: "b", Condition: ConditionStarted}) ||
		got[1] != (Dependency{Name: "c", Condition: ConditionStarted}) {
		t.Fatalf("list form = %+v", got)
	}
	if got := mapForm.Services["a"].DependsOn; len(got) != 2 ||
		got[0].Condition != ConditionStarted || got[1].Condition != ConditionHealthy {
		t.Fatalf("map form = %+v", got)
	}
	// Mapping order must be preserved, not alphabetised by Go map iteration.
	if mapForm.Services["a"].DependsOn[0].Name != "b" {
		t.Fatalf("mapping order lost: %+v", mapForm.Services["a"].DependsOn)
	}
}

func TestShellForms(t *testing.T) {
	cfg := parseOK(t, `
version: 1
project: {name: p}
services:
  a: {command: "x && y", shell: true}
  b: {command: "x", shell: false}
  c: {command: "x", shell: {type: powershell}}
`)
	if s := cfg.Services["a"].Shell; !s.Enabled || s.Type != ShellDefault {
		t.Fatalf("a shell = %+v", s)
	}
	if s := cfg.Services["b"].Shell; s.Enabled {
		t.Fatalf("b shell = %+v", s)
	}
	if s := cfg.Services["c"].Shell; !s.Enabled || s.Type != ShellPowerShell {
		t.Fatalf("c shell = %+v", s)
	}
}

func TestPortValueAndDurationForms(t *testing.T) {
	cfg := parseOK(t, `
version: 1
project: {name: p}
services:
  a:
    command: x
    ports:
      - {name: http, value: 8000}
      - {name: extra, value: auto}
    health: {type: process, interval: 7, timeout: 1500ms}
`)
	ports := cfg.Services["a"].Ports
	if ports[0].Value.Auto || ports[0].Value.Number != 8000 {
		t.Fatalf("fixed port = %+v", ports[0].Value)
	}
	if !ports[1].Value.Auto {
		t.Fatalf("auto port = %+v", ports[1].Value)
	}
	h := cfg.Services["a"].Health
	if h.Interval.Duration != 7*time.Second {
		t.Fatalf("interval = %v, want 7s (bare number means seconds)", h.Interval.Duration)
	}
	if h.Timeout.Duration != 1500*time.Millisecond {
		t.Fatalf("timeout = %v", h.Timeout.Duration)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	_, err := Parse([]byte(`
version: 1
project: {name: p}
services:
  a: {command: x, prot: 3000}
`))
	if !errs.Is(err, errs.CodeConfigInvalid) {
		t.Fatalf("err = %v, want CONFIG_INVALID", err)
	}
}

func TestPortSugarFormIsRejected(t *testing.T) {
	// `port:` (singular) is deliberately not part of the V0.1 schema: there is
	// exactly one canonical spelling, `ports:`.
	_, err := Parse([]byte(`
version: 1
project: {name: p}
services:
  a:
    command: x
    port: {env: PORT, value: auto}
`))
	if !errs.Is(err, errs.CodeConfigInvalid) {
		t.Fatalf("err = %v, want CONFIG_INVALID for `port:` sugar", err)
	}
}

func TestCommandMapFormIsRejected(t *testing.T) {
	// Platform differences use `platform.<os>.command`, never a command map.
	_, err := Parse([]byte(`
version: 1
project: {name: p}
services:
  a:
    command:
      default: pnpm
      windows: pnpm.cmd
`))
	if !errs.Is(err, errs.CodeConfigInvalid) {
		t.Fatalf("err = %v, want CONFIG_INVALID for command map", err)
	}
}

func TestValidateCanonicalConfig(t *testing.T) {
	cfg := parseOK(t, canonicalYAML)
	result := cfg.Validate(ValidateOptions{Platform: PlatformLinux})
	if !result.Valid {
		t.Fatalf("expected valid, errors = %v", result.Errors)
	}
}

func TestValidateShellWithArgs(t *testing.T) {
	cfg := parseOK(t, `
version: 1
project: {name: p}
services:
  a: {command: "pnpm dev", shell: true, args: [dev]}
`)
	result := cfg.Validate(ValidateOptions{})
	if result.Valid {
		t.Fatal("expected shell+args to be invalid")
	}
	if got := result.Errors[0].Path; got != "services.a.args" {
		t.Fatalf("path = %q", got)
	}
}

func TestValidateFixedPortCollision(t *testing.T) {
	cfg := parseOK(t, `
version: 1
project: {name: p}
services:
  a: {command: x, ports: [{name: http, value: 8000}]}
  b: {command: x, ports: [{name: http, value: 8000}]}
`)
	result := cfg.Validate(ValidateOptions{})
	if result.Valid {
		t.Fatal("expected static port collision")
	}
	if result.Errors[0].Code != errs.CodePortConflict {
		t.Fatalf("code = %s", result.Errors[0].Code)
	}
}

func TestValidateDependencyCycle(t *testing.T) {
	cfg := parseOK(t, `
version: 1
project: {name: p}
services:
  a: {command: x, depends_on: [b]}
  b: {command: x, depends_on: [c]}
  c: {command: x, depends_on: [a]}
`)
	result := cfg.Validate(ValidateOptions{})
	if result.Valid {
		t.Fatal("expected cycle detection")
	}
	if _, err := cfg.TopoOrder([]string{"a"}); err == nil {
		t.Fatal("TopoOrder should refuse a cyclic graph")
	}
}

func TestValidateUnknownDependency(t *testing.T) {
	cfg := parseOK(t, `
version: 1
project: {name: p}
services:
  a: {command: x, depends_on: [nope]}
`)
	if cfg.Validate(ValidateOptions{}).Valid {
		t.Fatal("expected unknown dependency error")
	}
}

func TestValidateTemplateReferences(t *testing.T) {
	cfg := parseOK(t, `
version: 1
project: {name: p}
services:
  a:
    command: x
    args: ["--port", "${PORT:nope}"]
    ports: [{name: http, value: auto}]
`)
	result := cfg.Validate(ValidateOptions{})
	if result.Valid {
		t.Fatal("expected undeclared port reference to fail")
	}
	if result.Errors[0].Path != "services.a.args[1]" {
		t.Fatalf("path = %q", result.Errors[0].Path)
	}

	noPorts := parseOK(t, `
version: 1
project: {name: p}
services:
  a: {command: x, args: ["${PORT}"]}
`)
	if noPorts.Validate(ValidateOptions{}).Valid {
		t.Fatal("expected ${PORT} without ports to fail")
	}

	envPort := parseOK(t, `
version: 1
project: {name: p}
services:
  a: {command: x, args: ["${ENV:PORT}"], ports: [{name: http, value: auto}]}
`)
	if envPort.Validate(ValidateOptions{}).Valid {
		t.Fatal("expected ${ENV:PORT} to be rejected in favour of ${PORT}")
	}
}

func TestValidateExternalRuntime(t *testing.T) {
	cfg := parseOK(t, `
version: 1
project: {name: p}
services:
  db: {runtime: external, command: postgres}
`)
	if cfg.Validate(ValidateOptions{}).Valid {
		t.Fatal("external runtime must not accept a command")
	}

	processHealth := parseOK(t, `
version: 1
project: {name: p}
services:
  db: {runtime: external}
`)
	if processHealth.Validate(ValidateOptions{}).Valid {
		t.Fatal("external runtime cannot use process health")
	}
}

func TestTemplateExpansion(t *testing.T) {
	ctx := TemplateContext{
		ProjectDir:      "/proj",
		ServiceDir:      "/proj/backend",
		Home:            "/home/dev",
		Ports:           map[string]int{"http": 8012, "debug": 5678},
		DefaultPortName: "http",
		Env: func(name string) (string, bool) {
			if name == "OPENAI_API_KEY" {
				return "sk-test", true
			}
			return "", false
		},
	}

	cases := map[string]string{
		"http://127.0.0.1:${PORT}/health": "http://127.0.0.1:8012/health",
		"${PORT:debug}":                   "5678",
		"${PROJECT_DIR}/x":                "/proj/x",
		"${SERVICE_DIR}":                  "/proj/backend",
		"${HOME}/.cache":                  "/home/dev/.cache",
		"key=${ENV:OPENAI_API_KEY}":       "key=sk-test",
		"literal $${PORT}":                "literal ${PORT}",
		"no vars":                         "no vars",
	}
	for input, want := range cases {
		got, err := ctx.Expand(input)
		if err != nil {
			t.Fatalf("Expand(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("Expand(%q) = %q, want %q", input, got, want)
		}
	}

	if _, err := ctx.Expand("${ENV:MISSING}"); !errs.Is(err, errs.CodeEnvMissing) {
		t.Fatalf("missing env err = %v, want ENV_MISSING", err)
	}
	if _, err := ctx.Expand("${NOPE}"); !errs.Is(err, errs.CodeConfigInvalid) {
		t.Fatalf("unknown var err = %v", err)
	}
	if _, err := ctx.Expand("${PORT"); !errs.Is(err, errs.CodeConfigInvalid) {
		t.Fatalf("unterminated err = %v", err)
	}
}

func TestPlatformOverlay(t *testing.T) {
	cfg := parseOK(t, `
version: 1
project: {name: p}
services:
  a:
    command: pnpm
    args: [dev]
    cwd: ./web
    env: {A: "1", B: "2"}
    platform:
      windows:
        command: pnpm.cmd
        env: {B: "windows"}
`)
	svc := cfg.Services["a"]

	linux := svc.Execution(PlatformLinux)
	if linux.Command != "pnpm" || linux.Env["B"] != "2" {
		t.Fatalf("linux exec = %+v", linux)
	}
	win := svc.Execution(PlatformWindows)
	if win.Command != "pnpm.cmd" || win.Env["B"] != "windows" || win.Env["A"] != "1" {
		t.Fatalf("windows exec = %+v", win)
	}
	if len(win.Args) != 1 || win.Args[0] != "dev" {
		t.Fatalf("overlay must not clear args: %+v", win.Args)
	}
}

func TestExecutionFingerprintIgnoresCosmeticChanges(t *testing.T) {
	base := parseOK(t, canonicalYAML)
	baseline := base.ExecutionFingerprint()

	cosmetic := parseOK(t, canonicalYAML)
	cosmetic.Services["frontend"].DisplayName = "Web UI"
	cosmetic.Services["frontend"].Health.Interval = NewDuration(30 * time.Second)
	cosmetic.Services["backend"].Restart.MaxAttempts = 99
	cosmetic.Services["backend"].Autostart = true
	if got := cosmetic.ExecutionFingerprint(); got != baseline {
		t.Fatal("cosmetic changes must not invalidate trust")
	}

	for name, mutate := range map[string]func(*Config){
		"command":  func(c *Config) { c.Services["frontend"].Command = "npm" },
		"args":     func(c *Config) { c.Services["frontend"].Args = []string{"start"} },
		"cwd":      func(c *Config) { c.Services["frontend"].CWD = "./other" },
		"shell":    func(c *Config) { c.Services["frontend"].Shell = ShellSpec{Enabled: true} },
		"env":      func(c *Config) { c.Services["backend"].Env["NODE_ENV"] = "production" },
		"env_file": func(c *Config) { c.Services["backend"].EnvFile = []string{".env.evil"} },
		"runtime":  func(c *Config) { c.Services["frontend"].Runtime = RuntimeExternal },
		"compose":  func(c *Config) { c.Services["redis"].Compose.Service = "other" },
		"platform": func(c *Config) {
			c.Services["frontend"].Platform = map[string]*PlatformOverlay{
				PlatformWindows: {Command: "evil.exe"},
			}
		},
		"new service": func(c *Config) {
			c.Services["evil"] = &Service{Name: "evil", Runtime: RuntimeHost, Command: "curl"}
		},
	} {
		mutated := parseOK(t, canonicalYAML)
		mutate(mutated)
		if mutated.ExecutionFingerprint() == baseline {
			t.Fatalf("changing %s must invalidate trust", name)
		}
	}
}

func TestExecutionFingerprintIsStableAcrossParses(t *testing.T) {
	a := parseOK(t, canonicalYAML).ExecutionFingerprint()
	for i := 0; i < 5; i++ {
		if parseOK(t, canonicalYAML).ExecutionFingerprint() != a {
			t.Fatal("fingerprint must be deterministic across parses")
		}
	}
}

func TestTopoOrderExpandsDependencies(t *testing.T) {
	cfg := parseOK(t, `
version: 1
project: {name: p}
services:
  worker: {command: x, depends_on: [backend]}
  backend: {command: x, depends_on: [redis, postgres]}
  redis: {command: x}
  postgres: {command: x}
  frontend: {command: x}
`)
	order, err := cfg.TopoOrder([]string{"worker"})
	if err != nil {
		t.Fatalf("TopoOrder: %v", err)
	}
	pos := map[string]int{}
	for i, name := range order {
		pos[name] = i
	}
	for _, pair := range [][2]string{{"redis", "backend"}, {"postgres", "backend"}, {"backend", "worker"}} {
		if pos[pair[0]] > pos[pair[1]] {
			t.Fatalf("%s must start before %s: %v", pair[0], pair[1], order)
		}
	}
	if _, ok := pos["frontend"]; ok {
		t.Fatalf("unrelated service pulled in: %v", order)
	}
}

func TestResolveServiceSet(t *testing.T) {
	cfg := parseOK(t, canonicalYAML)

	def, err := cfg.ResolveServiceSet(nil, "", false)
	if err != nil || len(def) != 2 || def[0] != "frontend" {
		t.Fatalf("startup.default = %v, %v", def, err)
	}
	all, err := cfg.ResolveServiceSet(nil, "", true)
	if err != nil || len(all) != 4 {
		t.Fatalf("--all = %v, %v", all, err)
	}
	full, err := cfg.ResolveServiceSet(nil, "full", false)
	if err != nil || len(full) != 3 {
		t.Fatalf("--profile full = %v, %v", full, err)
	}
	if _, err := cfg.ResolveServiceSet(nil, "nope", false); !errs.Is(err, errs.CodeConfigInvalid) {
		t.Fatalf("unknown profile err = %v", err)
	}
	if _, err := cfg.ResolveServiceSet([]string{"nope"}, "", false); !errs.Is(err, errs.CodeServiceNotFound) {
		t.Fatalf("unknown service err = %v", err)
	}
}

func TestDiscoveryOrderAndProjectRoot(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, ".devman")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	minimal := "version: 1\nproject: {name: p}\nservices:\n  a: {command: x}\n"

	// Only .devman/devman.yml present.
	if err := os.WriteFile(filepath.Join(nested, "devman.yml"), []byte(minimal), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ProjectRoot != root {
		t.Fatalf("project root = %q, want %q (.devman must not become the root)", cfg.ProjectRoot, root)
	}

	// Root devman.yaml wins over everything else.
	if err := os.WriteFile(filepath.Join(root, "devman.yaml"), []byte(minimal), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if filepath.Base(cfg.ConfigPath) != "devman.yaml" || filepath.Dir(cfg.ConfigPath) != root {
		t.Fatalf("config path = %q", cfg.ConfigPath)
	}

	if _, err := Load(t.TempDir()); !errs.Is(err, errs.CodeConfigNotFound) {
		t.Fatalf("missing config err = %v, want CONFIG_NOT_FOUND", err)
	}
}

func TestValidateFilesystemChecks(t *testing.T) {
	root := t.TempDir()
	yamlText := "version: 1\nproject: {name: p}\nservices:\n  a: {command: x, cwd: ./missing}\n"
	if err := os.WriteFile(filepath.Join(root, "devman.yaml"), []byte(yamlText), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	result := cfg.Validate(ValidateOptions{CheckFilesystem: true})
	if result.Valid {
		t.Fatal("expected missing cwd to be an error")
	}
	if result.Errors[0].Path != "services.a.cwd" {
		t.Fatalf("path = %q", result.Errors[0].Path)
	}
}

func TestExplainExecution(t *testing.T) {
	cfg := parseOK(t, canonicalYAML)
	summaries := cfg.ExplainExecution(PlatformLinux)
	if len(summaries) != 4 {
		t.Fatalf("summaries = %d", len(summaries))
	}
	if summaries[0].CommandLine != "pnpm dev" || summaries[0].CWD != "./frontend" {
		t.Fatalf("frontend summary = %+v", summaries[0])
	}
	if summaries[2].Compose != "docker-compose.yml / redis" {
		t.Fatalf("redis summary = %+v", summaries[2])
	}
}
