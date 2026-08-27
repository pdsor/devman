// Package cli implements the devman command line.
//
// The CLI is a presentation layer and nothing more: it resolves flags, calls the
// daemon API and renders DTOs. Every piece of state it shows comes from the API,
// which is what lets the GUI and the Codex skill behave identically without
// re-implementing anything.
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/devman-project/devman/internal/client"
	"github.com/devman-project/devman/internal/paths"
	"github.com/devman-project/devman/pkg/dto"
	"github.com/devman-project/devman/pkg/errs"
)

// exit codes. They are deliberately few: scripts branch on the JSON error code,
// not on a numeric taxonomy.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// App is one CLI invocation.
type App struct {
	Version string
	Layout  paths.Layout

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// JSON switches every command to machine readable output.
	JSON bool

	api *client.Client
}

// Main runs the CLI and returns a process exit code.
func Main(version string, args []string) int {
	layout, err := paths.Resolve()
	if err != nil {
		fmt.Fprintln(os.Stderr, "devman: cannot resolve the data directory:", err)
		return exitError
	}
	app := &App{
		Version: version,
		Layout:  layout,
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	}
	return app.Run(args)
}

// Run dispatches one command.
func (a *App) Run(args []string) int {
	if len(args) == 0 {
		a.usage()
		return exitUsage
	}

	// --json is accepted anywhere so `devman start --json backend` works as
	// naturally as `devman --json start backend`.
	command, rest := a.extractGlobals(args)

	switch command {
	case "help", "-h", "--help":
		a.usage()
		return exitOK
	case "version", "-v", "--version":
		return a.report(a.cmdVersion(rest))
	case "init":
		return a.report(a.cmdInit(rest))
	case "validate":
		return a.report(a.cmdValidate(rest))
	case "paths":
		return a.report(a.cmdPaths(rest))
	case "register":
		return a.report(a.cmdRegister(rest))
	case "unregister":
		return a.report(a.cmdUnregister(rest))
	case "list", "ls":
		return a.report(a.cmdList(rest))
	case "status":
		return a.report(a.cmdStatus(rest))
	case "start":
		return a.report(a.cmdStart(rest))
	case "stop":
		return a.report(a.cmdStop(rest))
	case "restart":
		return a.report(a.cmdRestart(rest))
	case "logs":
		return a.report(a.cmdLogs(rest))
	case "events":
		return a.report(a.cmdEvents(rest))
	case "ports":
		return a.report(a.cmdPorts(rest))
	case "open":
		return a.report(a.cmdOpen(rest))
	case "trust":
		return a.report(a.cmdTrust(rest))
	case "config":
		return a.report(a.cmdConfig(rest))
	case "daemon":
		return a.report(a.cmdDaemon(rest))
	default:
		fmt.Fprintf(a.Stderr, "devman: unknown command %q\n\n", command)
		a.usage()
		return exitUsage
	}
}

// extractGlobals pulls global flags out of the argument list wherever they
// appear and returns the command plus the remaining arguments.
func (a *App) extractGlobals(args []string) (string, []string) {
	var (
		command string
		rest    []string
	)
	for _, arg := range args {
		switch arg {
		case "--json":
			a.JSON = true
			continue
		}
		if command == "" && !strings.HasPrefix(arg, "-") {
			command = arg
			continue
		}
		rest = append(rest, arg)
	}
	if command == "" && len(args) > 0 {
		command = args[0]
	}
	return command, rest
}

// --- flag handling ---

// parse parses a command's flags, allowing them to appear after positional
// arguments.
//
// Go's flag package stops at the first non-flag argument, which would make
// `devman logs backend --follow` silently ignore --follow. Users type commands
// that way, and an ignored flag is worse than an error, so the arguments are
// permuted before parsing.
func (a *App) parse(flags *flag.FlagSet, args []string) error {
	ordered, positional := permute(flags, args)
	if len(positional) > 0 {
		// The terminator keeps a positional that begins with a dash — a service
		// named oddly, or an argument after `--` — from being read as a flag.
		ordered = append(ordered, "--")
		ordered = append(ordered, positional...)
	}
	if err := flags.Parse(ordered); err != nil {
		return errs.New(errs.CodeInvalidRequest, "%v", err)
	}
	return nil
}

// permute splits arguments into flags (with their values) and positionals,
// preserving the order within each group.
func permute(flags *flag.FlagSet, args []string) (flagArgs, positional []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			// Everything after `--` is positional by convention.
			positional = append(positional, args[i+1:]...)
			return flagArgs, positional
		}
		if len(arg) < 2 || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flagArgs = append(flagArgs, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue
		}
		// A non-boolean flag consumes the next argument as its value; an unknown
		// flag is left alone so Parse can report it.
		found := flags.Lookup(name)
		if found == nil || isBoolFlag(found) {
			continue
		}
		if i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return flagArgs, positional
}

func isBoolFlag(found *flag.Flag) bool {
	boolFlag, ok := found.Value.(interface{ IsBoolFlag() bool })
	return ok && boolFlag.IsBoolFlag()
}

// report renders an error and maps it onto an exit code.
func (a *App) report(err error) int {
	if err == nil {
		return exitOK
	}
	converted := errs.From(err)
	if a.JSON {
		a.printJSON(map[string]any{"error": dto.FromError(converted)})
		return exitError
	}
	fmt.Fprintf(a.Stderr, "devman: %s\n", converted.Message)
	if converted.Path != "" {
		fmt.Fprintf(a.Stderr, "  at %s\n", converted.Path)
	}
	fmt.Fprintf(a.Stderr, "  code %s\n", converted.Code)
	if hint := hintFor(converted); hint != "" {
		fmt.Fprintf(a.Stderr, "  %s\n", hint)
	}
	return exitError
}

// hintFor turns the common failures into the next thing to actually do.
func hintFor(err *errs.Error) string {
	switch err.Code {
	case errs.CodeDaemonNotRunning:
		return "try: devman daemon start"
	case errs.CodeProjectUntrusted:
		return "review the commands and approve them: devman trust"
	case errs.CodeConfigNotFound:
		return "create a configuration first: devman init"
	case errs.CodeProjectNotFound:
		return "register the project first: devman register ."
	case errs.CodePortConflict:
		return "see who holds the port: devman ports"
	case errs.CodeServiceBlocked:
		return "the service is missing a prerequisite; nothing was started"
	default:
		return ""
	}
}

// --- daemon access ---

// client returns a client, starting the daemon if it is not running.
//
// Auto-start is what makes the daemon an implementation detail: a user should
// never have to know it exists.
func (a *App) client() (*client.Client, error) {
	if a.api != nil {
		return a.api, nil
	}
	executable, err := os.Executable()
	if err != nil {
		executable = ""
	}
	api, err := client.AutoStart(a.Layout, executable, 20*time.Second)
	if err != nil {
		return nil, err
	}
	a.api = api
	return api, nil
}

// connect returns a client without starting anything, for commands that must
// report the daemon as stopped rather than start it.
func (a *App) connect() (*client.Client, error) {
	if a.api != nil {
		return a.api, nil
	}
	api, err := client.Connect(a.Layout)
	if err != nil {
		return nil, err
	}
	a.api = api
	return api, nil
}

// resolveProject turns a selector, or the current directory, into a project.
func (a *App) resolveProject(api *client.Client, selector string) (dto.Project, error) {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	return api.Resolve(selector, cwd)
}

// --- shared rendering ---

func (a *App) printf(format string, args ...any) {
	fmt.Fprintf(a.Stdout, format, args...)
}

func (a *App) println(args ...any) {
	fmt.Fprintln(a.Stdout, args...)
}

// serviceRows renders services as a table, sorted by name for stable output.
func (a *App) printServices(services []dto.Service) {
	if len(services) == 0 {
		a.println("no services")
		return
	}
	sorted := append([]dto.Service(nil), services...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	table := newTable("SERVICE", "STATUS", "HEALTH", "PID", "PORTS", "UPTIME", "URL")
	for _, svc := range sorted {
		table.add(
			svc.Name,
			statusLabel(svc),
			string(svc.Health.Status),
			pidLabel(svc.PID),
			portsLabel(svc.Ports),
			uptimeLabel(svc.UptimeSeconds),
			svc.URL,
		)
	}
	table.render(a.Stdout)

	// A blocked or failed service is useless without the reason, so it is
	// printed under the table rather than truncated into a column.
	for _, svc := range sorted {
		if svc.Message != "" && svc.Status != dto.StatusRunning {
			a.printf("\n%s: %s\n", svc.Name, svc.Message)
		}
	}
}

// statusLabel annotates a status with the facts that change its meaning.
func statusLabel(svc dto.Service) string {
	label := string(svc.Status)
	if svc.Observability.Adopted {
		// An adopted service is running but unwatched, which the user has to
		// know before wondering why `devman logs` is quiet.
		label += " (adopted)"
	}
	if svc.Status == dto.StatusRunning && svc.Observability.LogCapture == dto.LogCaptureDetached {
		label += " (no log capture)"
	}
	if svc.RestartCount > 0 {
		label += fmt.Sprintf(" ×%d", svc.RestartCount)
	}
	return label
}

func pidLabel(pid int) string {
	if pid == 0 {
		return "-"
	}
	return fmt.Sprint(pid)
}

func portsLabel(ports []dto.PortAllocation) string {
	if len(ports) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(ports))
	for _, port := range ports {
		text := fmt.Sprintf("%s=%d", port.Name, port.Port)
		if port.Status == dto.PortUnverified {
			// The service never bound what it was given; saying so is the whole
			// point of tracking the state.
			text += "?"
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, " ")
}

func uptimeLabel(seconds int64) string {
	if seconds <= 0 {
		return "-"
	}
	duration := time.Duration(seconds) * time.Second
	switch {
	case duration < time.Minute:
		return fmt.Sprintf("%ds", int(duration.Seconds()))
	case duration < time.Hour:
		return fmt.Sprintf("%dm", int(duration.Minutes()))
	case duration < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(duration.Hours()), int(duration.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(duration.Hours())/24, int(duration.Hours())%24)
	}
}

func (a *App) usage() {
	fmt.Fprint(a.Stderr, `devman — local development runtime manager

Usage:
  devman <command> [arguments] [--json]

Project commands:
  init                     write a devman.yaml for the current directory
  validate [path]          check a configuration without running anything
  register [path]          register a project after reviewing what it may run
  unregister [project]     forget a project
  list                     list registered projects
  status [project]         show services, health, ports and URLs
  trust [project]          approve the project's current commands

Service commands:
  start [services...]      start services in dependency order
  stop [services...]       stop services and remember that they must stay stopped
  restart [services...]    restart services, re-reading devman.yaml
  logs <service>           show captured output (--follow to stream)
  open [service]           open the GUI, or a service's HTTP port
  ports [port]             show port allocations, or who holds one port

Daemon and environment:
  daemon start|stop|status|restart
  events                   show recent daemon events (--follow to stream)
  config list|get|set      read and write the global settings
  paths                    show where DevMan keeps its data
  version                  print the version

Common flags:
  --json                   machine readable output for every command
  --project <selector>     act on a project by id, name or path
  --all                    apply to every service of the project
  --profile <name>         use a declared service profile
`)
}
