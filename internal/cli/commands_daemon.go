package cli

import (
	"sort"
	"strconv"
	"time"

	"github.com/devman-project/devman/internal/client"
	"github.com/devman-project/devman/internal/daemon"
	"github.com/devman-project/devman/pkg/errs"
)

// cmdDaemon manages the daemon explicitly.
//
// Every other command auto-starts it, so these subcommands exist for the cases
// where the daemon itself is the subject: diagnosing it, restarting it after an
// upgrade, or shutting everything down deliberately.
func (a *App) cmdDaemon(args []string) error {
	if len(args) == 0 {
		return a.daemonStatus(nil)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "start":
		return a.daemonStart(rest)
	case "stop":
		return a.daemonStop(rest)
	case "status":
		return a.daemonStatus(rest)
	case "restart":
		return a.daemonRestart(rest)
	default:
		return errs.New(errs.CodeInvalidRequest,
			"unknown daemon command %q; use start, stop, status or restart", sub)
	}
}

// daemonStart runs the daemon, either in this process or as a detached child.
func (a *App) daemonStart(args []string) error {
	flags := a.newFlags("daemon start")
	foreground := flags.Bool("foreground", false, "run the daemon in this process")
	if err := a.parse(flags, args); err != nil {
		return err
	}

	if *foreground {
		// This is the process the CLI spawns when it auto-starts the daemon, and
		// the one a user runs to watch the daemon's own output.
		return daemon.Run(a.Layout, a.Version, a.Stderr)
	}

	if existing, err := a.connect(); err == nil {
		status, statusErr := existing.DaemonStatus()
		if statusErr == nil {
			if a.JSON {
				a.printJSON(status)
				return nil
			}
			a.printf("daemon already running on port %d (pid %d)\n",
				status.Info.Port, status.Info.PID)
			return nil
		}
	}

	api, err := a.client()
	if err != nil {
		return err
	}
	status, err := api.DaemonStatus()
	if err != nil {
		return err
	}
	if a.JSON {
		a.printJSON(status)
		return nil
	}
	a.printf("daemon started on port %d (pid %d)\n", status.Info.Port, status.Info.PID)
	return nil
}

// daemonStop stops the daemon, and with it every service it manages.
//
// Stopping the daemon stops the services: leaving orphaned processes behind
// would mean DevMan no longer knows what is running, which is the one state a
// process manager must never leave the machine in.
func (a *App) daemonStop(args []string) error {
	flags := a.newFlags("daemon stop")
	if err := a.parse(flags, args); err != nil {
		return err
	}

	api, err := a.connect()
	if err != nil {
		if errs.From(err).Code == errs.CodeDaemonNotRunning {
			if a.JSON {
				a.printJSON(map[string]string{"daemon": "not running"})
				return nil
			}
			a.println("daemon is not running")
			return nil
		}
		return err
	}

	result, err := api.Shutdown()
	if err != nil {
		return err
	}
	if a.JSON {
		a.printJSON(result)
		return nil
	}
	if len(result.Services) == 0 {
		a.println("daemon stopped")
		return nil
	}
	a.printf("stopped %d service(s), then the daemon\n", len(result.Services))
	for _, svc := range result.Services {
		a.printf("  %s/%s %s\n", svc.Project, svc.Name, svc.Status)
	}
	return nil
}

func (a *App) daemonRestart(args []string) error {
	flags := a.newFlags("daemon restart")
	if err := a.parse(flags, args); err != nil {
		return err
	}
	if err := a.daemonStop(nil); err != nil {
		return err
	}
	// The old daemon has to release its port and its daemon.json before a new
	// one can bind, so wait for discovery to go quiet rather than racing it.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := client.Connect(a.Layout); err != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	a.api = nil
	return a.daemonStart(nil)
}

func (a *App) daemonStatus(args []string) error {
	flags := a.newFlags("daemon status")
	if err := a.parse(flags, args); err != nil {
		return err
	}

	// Deliberately no auto-start: a status command that starts what it reports
	// on can never tell you that it was stopped.
	api, err := a.connect()
	if err != nil {
		if errs.From(err).Code == errs.CodeDaemonNotRunning {
			if a.JSON {
				a.printJSON(map[string]any{"running": false})
				return nil
			}
			a.println("daemon is not running")
			a.println("next: devman daemon start")
			return nil
		}
		return err
	}
	status, err := api.DaemonStatus()
	if err != nil {
		return err
	}
	if a.JSON {
		a.printJSON(status)
		return nil
	}
	table := newTable("WHAT", "VALUE")
	table.add("state", "running")
	table.add("version", status.Info.Version)
	table.add("api", status.Info.APIVersion)
	table.add("pid", itoa(status.Info.PID))
	table.add("address", status.Info.Host+":"+itoa(status.Info.Port))
	table.add("uptime", uptimeLabel(status.Uptime))
	table.add("projects", itoa(status.Projects))
	table.add("running services", itoa(status.Running))
	table.add("data directory", status.DataDir)
	table.add("logs", status.LogsDir)
	if !status.Info.GracefulSignals {
		// Without a console DevMan cannot send CTRL_BREAK, so stops become force
		// kills. A user debugging why a service lost unsaved state needs to know.
		table.add("graceful stop", "unavailable (no console): services are force stopped")
	}
	table.render(a.Stdout)
	return nil
}

// cmdConfig reads and writes the global settings.
func (a *App) cmdConfig(args []string) error {
	if len(args) == 0 {
		return a.configList(nil)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return a.configList(rest)
	case "get":
		return a.configGet(rest)
	case "set":
		return a.configSet(rest)
	default:
		return errs.New(errs.CodeInvalidRequest,
			"unknown config command %q; use list, get or set", sub)
	}
}

func (a *App) configList(args []string) error {
	flags := a.newFlags("config list")
	if err := a.parse(flags, args); err != nil {
		return err
	}
	api, err := a.client()
	if err != nil {
		return err
	}
	values, err := api.Settings()
	if err != nil {
		return err
	}
	if a.JSON {
		a.printJSON(values)
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	table := newTable("KEY", "VALUE")
	for _, key := range keys {
		table.add(key, values[key])
	}
	table.render(a.Stdout)
	return nil
}

func (a *App) configGet(args []string) error {
	flags := a.newFlags("config get")
	if err := a.parse(flags, args); err != nil {
		return err
	}
	if flags.NArg() == 0 {
		return errs.New(errs.CodeInvalidRequest, "devman config get needs a key")
	}
	api, err := a.client()
	if err != nil {
		return err
	}
	key := flags.Arg(0)
	value, err := api.Setting(key)
	if err != nil {
		return err
	}
	if a.JSON {
		a.printJSON(map[string]string{key: value})
		return nil
	}
	a.println(value)
	return nil
}

func (a *App) configSet(args []string) error {
	flags := a.newFlags("config set")
	if err := a.parse(flags, args); err != nil {
		return err
	}
	if flags.NArg() < 2 {
		return errs.New(errs.CodeInvalidRequest, "devman config set needs a key and a value")
	}
	api, err := a.client()
	if err != nil {
		return err
	}
	key, value := flags.Arg(0), flags.Arg(1)
	if err := api.SetSetting(key, value); err != nil {
		return err
	}
	if a.JSON {
		a.printJSON(map[string]string{key: value})
		return nil
	}
	a.printf("%s = %s\n", key, value)
	return nil
}

func itoa(value int) string { return strconv.Itoa(value) }
