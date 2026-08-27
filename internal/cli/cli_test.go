package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devman-project/devman/internal/paths"
	"github.com/devman-project/devman/pkg/dto"
	"github.com/devman-project/devman/pkg/errs"
)

// These tests cover the CLI as a presentation layer: flag handling, the local
// commands that must work before a daemon exists, and the rendering decisions
// that change what a user believes about a service. The full chain through a
// live daemon is exercised by internal/acceptance.

func newApp(t *testing.T) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	app := &App{
		Version: "test",
		Layout:  paths.For(t.TempDir()),
		Stdin:   strings.NewReader(""),
		Stdout:  &stdout,
		Stderr:  &stderr,
	}
	return app, &stdout, &stderr
}

func TestGlobalJSONFlagIsAcceptedAnywhere(t *testing.T) {
	app, _, _ := newApp(t)
	command, rest := app.extractGlobals([]string{"start", "--json", "backend"})
	if command != "start" {
		t.Fatalf("command = %q", command)
	}
	if !app.JSON {
		t.Fatal("--json after the command must still be honoured")
	}
	if len(rest) != 1 || rest[0] != "backend" {
		t.Fatalf("rest = %v", rest)
	}
}

// TestFlagsAreAcceptedAfterPositionalArguments locks in the permutation: users
// type `devman logs backend --follow`, and Go's flag package would otherwise stop
// parsing at "backend" and silently ignore the flag.
func TestFlagsAreAcceptedAfterPositionalArguments(t *testing.T) {
	app, _, _ := newApp(t)
	flags := app.newFlags("logs")
	follow := flags.Bool("follow", false, "")
	project := flags.String("project", "", "")
	tail := flags.Int("tail", 0, "")

	if err := app.parse(flags, []string{"backend", "--follow", "--project", "web", "--tail=25"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !*follow || *project != "web" || *tail != 25 {
		t.Fatalf("follow=%v project=%q tail=%d", *follow, *project, *tail)
	}
	if flags.NArg() != 1 || flags.Arg(0) != "backend" {
		t.Fatalf("positional arguments = %v", flags.Args())
	}

	// Everything after `--` stays positional even if it looks like a flag.
	app2, _, _ := newApp(t)
	passthrough := app2.newFlags("start")
	all := passthrough.Bool("all", false, "")
	if err := app2.parse(passthrough, []string{"--all", "--", "--not-a-flag"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !*all || passthrough.NArg() != 1 || passthrough.Arg(0) != "--not-a-flag" {
		t.Fatalf("all=%v args=%v", *all, passthrough.Args())
	}
}

func TestInitWritesAConfigThatValidates(t *testing.T) {
	app, stdout, _ := newApp(t)
	root := t.TempDir()

	if err := app.cmdInit([]string{root}); err != nil {
		t.Fatalf("init: %v", err)
	}
	written := filepath.Join(root, "devman.yaml")
	if _, err := os.Stat(written); err != nil {
		t.Fatalf("init did not write %s: %v", written, err)
	}
	body, err := os.ReadFile(written)
	if err != nil {
		t.Fatal(err)
	}
	// The generated file is the example most users copy from, so it must not
	// contain the spellings DevMan rejects.
	text := string(body)
	for _, forbidden := range []string{"\n    port:", "port: {"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("the scaffold must not use `port:` sugar:\n%s", text)
		}
	}

	// A scaffold that does not pass `devman validate` would be a trap.
	if err := app.cmdValidate([]string{root}); err != nil {
		t.Fatalf("the generated config must validate: %v", err)
	}
	if !strings.Contains(stdout.String(), "valid") {
		t.Fatalf("validate said nothing useful: %q", stdout.String())
	}

	// A second init must not silently replace a file the user has edited.
	if err := app.cmdInit([]string{root}); !errs.Is(err, errs.CodeProjectExists) {
		t.Fatalf("expected PROJECT_EXISTS without --force, got %v", err)
	}
}

func TestValidateReportsInvalidConfiguration(t *testing.T) {
	app, stdout, _ := newApp(t)
	root := t.TempDir()
	body := `version: 1
project: {name: p}
services:
  a: {command: "pnpm dev", shell: true, args: [dev]}
`
	if err := os.WriteFile(filepath.Join(root, "devman.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	err := app.cmdValidate([]string{root})
	if !errs.Is(err, errs.CodeConfigInvalid) {
		t.Fatalf("expected CONFIG_INVALID, got %v", err)
	}
	if !strings.Contains(stdout.String(), "services.a.args") {
		t.Fatalf("validate must point at the offending field:\n%s", stdout.String())
	}
}

func TestValidateJSONIsMachineReadable(t *testing.T) {
	app, stdout, _ := newApp(t)
	app.JSON = true
	root := t.TempDir()
	if err := app.cmdInit([]string{root}); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()

	if err := app.cmdValidate([]string{root}); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var result struct {
		Valid bool `json:"valid"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, stdout.String())
	}
	if !result.Valid {
		t.Fatalf("expected valid: true, got %s", stdout.String())
	}
}

func TestPathsWorkWithoutADaemon(t *testing.T) {
	app, stdout, _ := newApp(t)
	app.JSON = true
	if err := app.cmdPaths(nil); err != nil {
		t.Fatalf("paths: %v", err)
	}
	var value dto.Paths
	if err := json.Unmarshal(stdout.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value.Home != app.Layout.Home || value.AuthToken == "" {
		t.Fatalf("paths did not report the resolved layout: %+v", value)
	}
}

func TestErrorsAreReportedWithACodeAndAHint(t *testing.T) {
	app, _, stderr := newApp(t)
	code := app.report(errs.New(errs.CodeDaemonNotRunning, "nothing is listening"))
	if code != exitError {
		t.Fatalf("exit code = %d", code)
	}
	text := stderr.String()
	if !strings.Contains(text, "DAEMON_NOT_RUNNING") {
		t.Fatalf("the machine readable code must be shown:\n%s", text)
	}
	if !strings.Contains(text, "devman daemon start") {
		t.Fatalf("a recoverable error must say what to do next:\n%s", text)
	}
}

func TestJSONErrorsCarryTheCode(t *testing.T) {
	app, stdout, _ := newApp(t)
	app.JSON = true
	app.report(errs.New(errs.CodePortConflict, "3000 is taken").At("services.web.ports[0]"))

	var payload struct {
		Error dto.Error `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("error output is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.Error.Code != string(errs.CodePortConflict) {
		t.Fatalf("code = %q", payload.Error.Code)
	}
	if payload.Error.Path != "services.web.ports[0]" {
		t.Fatalf("path = %q", payload.Error.Path)
	}
}

func TestStatusLabelSaysWhatDevManCannotSee(t *testing.T) {
	adopted := dto.Service{
		Name:   "api",
		Status: dto.StatusRunning,
		Observability: dto.Observability{
			Adopted:    true,
			LogCapture: dto.LogCaptureDetached,
		},
		RestartCount: 2,
	}
	label := statusLabel(adopted)
	for _, want := range []string{"RUNNING", "adopted", "no log capture", "×2"} {
		if !strings.Contains(label, want) {
			t.Fatalf("label %q is missing %q", label, want)
		}
	}

	plain := statusLabel(dto.Service{Status: dto.StatusRunning,
		Observability: dto.Observability{LogCapture: dto.LogCaptureAttached}})
	if plain != "RUNNING" {
		t.Fatalf("a normal service must render plainly, got %q", plain)
	}
}

func TestPortsLabelMarksUnverifiedPorts(t *testing.T) {
	label := portsLabel([]dto.PortAllocation{
		{Name: "http", Port: 3000, Status: dto.PortBound},
		{Name: "debug", Port: 9229, Status: dto.PortUnverified},
	})
	if !strings.Contains(label, "http=3000") {
		t.Fatalf("label = %q", label)
	}
	if !strings.Contains(label, "debug=9229?") {
		t.Fatalf("an unverified port must be marked, got %q", label)
	}
}

func TestServiceURLRequiresAChoiceWhenAmbiguous(t *testing.T) {
	both := dto.Service{
		Name: "web",
		Ports: []dto.PortAllocation{
			{Name: "api", Port: 8000},
			{Name: "metrics", Port: 9000},
		},
	}
	if _, err := serviceURL(both, ""); !errs.Is(err, errs.CodeInvalidRequest) {
		t.Fatalf("expected an explicit choice to be required, got %v", err)
	}
	url, err := serviceURL(both, "metrics")
	if err != nil || !strings.HasSuffix(url, ":9000") {
		t.Fatalf("serviceURL = %q, %v", url, err)
	}
	if _, err := serviceURL(both, "nope"); !errs.Is(err, errs.CodeInvalidRequest) {
		t.Fatalf("an unknown port name must be an error, got %v", err)
	}

	preferred := dto.Service{Ports: []dto.PortAllocation{
		{Name: "debug", Port: 9229},
		{Name: "http", Port: 3000},
	}}
	url, err = serviceURL(preferred, "")
	if err != nil || !strings.HasSuffix(url, ":3000") {
		t.Fatalf("the port named http must win by default, got %q, %v", url, err)
	}

	if _, err := serviceURL(dto.Service{Name: "worker"}, ""); !errs.Is(err, errs.CodeInvalidRequest) {
		t.Fatalf("a service without ports has nothing to open, got %v", err)
	}
}

func TestUptimeLabelIsReadable(t *testing.T) {
	cases := map[int64]string{
		0:      "-",
		45:     "45s",
		600:    "10m",
		3720:   "1h2m",
		176400: "2d1h",
	}
	for seconds, want := range cases {
		if got := uptimeLabel(seconds); got != want {
			t.Fatalf("uptimeLabel(%d) = %q, want %q", seconds, got, want)
		}
	}
}

func TestTableDropsEmptyTrailingColumns(t *testing.T) {
	var out bytes.Buffer
	table := newTable("SERVICE", "STATUS", "URL")
	table.add("api", "RUNNING", "")
	table.add("worker", "RUNNING", "")
	table.render(&out)
	if strings.Contains(out.String(), "URL") {
		t.Fatalf("a column with no values must not be printed:\n%s", out.String())
	}
}

func TestUnknownCommandIsAUsageError(t *testing.T) {
	app, _, stderr := newApp(t)
	if code := app.Run([]string{"frobnicate"}); code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestOperationErrorSurfacesTheFirstFailure(t *testing.T) {
	app, _, _ := newApp(t)
	err := app.operationError(dto.OperationResult{
		Errors: []dto.Error{
			{Code: string(errs.CodeServiceBlocked), Message: "docker is not installed"},
			{Code: string(errs.CodePortConflict), Message: "3000 is taken"},
		},
	})
	if !errs.Is(err, errs.CodeServiceBlocked) {
		t.Fatalf("err = %v, want SERVICE_BLOCKED", err)
	}
	if !strings.Contains(err.Error(), "1 more") {
		t.Fatalf("the remaining failures must be acknowledged: %v", err)
	}
	if app.operationError(dto.OperationResult{}) != nil {
		t.Fatal("a clean result must not produce an error")
	}
}
