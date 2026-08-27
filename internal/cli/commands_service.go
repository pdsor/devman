package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/devman-project/devman/internal/client"
	"github.com/devman-project/devman/pkg/dto"
	"github.com/devman-project/devman/pkg/errs"
)

// The service commands are all the same shape: resolve a project, build a
// selection, call the API, render the returned services. Keeping the selection
// in one helper is what makes `devman start`, `devman stop backend` and
// `devman restart --all` behave identically about which services they touch.

func (a *App) cmdStart(args []string) error {
	return a.runProjectAction("start", args)
}

func (a *App) cmdStop(args []string) error {
	return a.runProjectAction("stop", args)
}

func (a *App) cmdRestart(args []string) error {
	return a.runProjectAction("restart", args)
}

// runProjectAction performs one of start/stop/restart.
//
// Named services are always routed through the project endpoint rather than the
// single-service one, because dependency ordering is a project-level concern:
// starting "backend" may require starting "database" first, and the daemon is
// the only component that knows that.
func (a *App) runProjectAction(action string, args []string) error {
	flags := a.newFlags(action)
	project := flags.String("project", "", "project id, name or path")
	profile := flags.String("profile", "", "declared service profile")
	all := flags.Bool("all", false, "apply to every service")
	wait := flags.Duration("wait", 0, "wait for services to become healthy")
	if err := a.parse(flags, args); err != nil {
		return err
	}

	api, err := a.client()
	if err != nil {
		return err
	}
	record, err := a.resolveProject(api, *project)
	if err != nil {
		return err
	}

	selection := client.Selection{
		Services: flags.Args(),
		Profile:  *profile,
		All:      *all,
	}

	var result dto.OperationResult
	switch action {
	case "start":
		result, err = api.StartProject(record.ID, selection)
	case "stop":
		result, err = api.StopProject(record.ID, selection)
	case "restart":
		result, err = api.RestartProject(record.ID, selection)
	default:
		return errs.New(errs.CodeInternal, "unknown action %q", action)
	}
	if err != nil {
		return err
	}

	if *wait > 0 && action != "stop" {
		waited, waitErr := a.awaitHealthy(api, record.ID, result, *wait)
		if waitErr != nil {
			return waitErr
		}
		result.Services = waited
	}

	if a.JSON {
		a.printJSON(result)
		return a.operationError(result)
	}
	a.printServices(result.Services)
	for _, failure := range result.Errors {
		a.printf("\n%s: %s\n", failure.Code, failure.Message)
	}
	return a.operationError(result)
}

// operationError turns per-service failures into a non-zero exit code without
// hiding the services that did come up: the table is printed either way.
func (a *App) operationError(result dto.OperationResult) error {
	if len(result.Errors) == 0 {
		return nil
	}
	first := result.Errors[0]
	if len(result.Errors) == 1 {
		return errs.New(errs.Code(first.Code), "%s", first.Message)
	}
	return errs.New(errs.Code(first.Code),
		"%s (and %d more failure(s))", first.Message, len(result.Errors)-1)
}

// awaitHealthy polls until every started service is healthy, or the deadline
// passes. Polling the API rather than watching events keeps this honest: the
// answer is the same state any other client would see.
func (a *App) awaitHealthy(
	api *client.Client, projectID string, result dto.OperationResult, timeout time.Duration,
) ([]dto.Service, error) {
	names := make(map[string]bool, len(result.Services))
	for _, svc := range result.Services {
		names[svc.Name] = true
	}
	deadline := time.Now().Add(timeout)
	latest := result.Services
	for {
		services, err := api.Services(projectID)
		if err != nil {
			return latest, err
		}
		selected := make([]dto.Service, 0, len(names))
		settled := true
		for _, svc := range services {
			if !names[svc.Name] {
				continue
			}
			selected = append(selected, svc)
			if !serviceSettled(svc) {
				settled = false
			}
		}
		latest = selected
		if settled || !time.Now().Before(deadline) {
			return latest, nil
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// serviceSettled reports whether a service has reached the state `--wait` is
// waiting for.
//
// A RUNNING service that is not yet healthy is deliberately *not* settled: a
// health probe usually fails once or twice while a dev server boots, and
// returning at the first failure would make `--wait` report a service as broken
// moments before it comes up.
func serviceSettled(svc dto.Service) bool {
	switch svc.Status {
	case dto.StatusStarting, dto.StatusStopping:
		return false
	case dto.StatusRunning:
		switch svc.Health.Status {
		case dto.HealthHealthy, dto.HealthNotApplicable:
			return true
		default:
			return false
		}
	default:
		// FAILED, BLOCKED, CRASHED and STOPPED will not change on their own.
		return true
	}
}

// cmdLogs shows captured output for one service.
func (a *App) cmdLogs(args []string) error {
	flags := a.newFlags("logs")
	project := flags.String("project", "", "project id, name or path")
	follow := flags.Bool("follow", false, "stream new output until interrupted")
	short := flags.Bool("f", false, "alias for --follow")
	tail := flags.Int("tail", 200, "how many recent lines to show")
	stream := flags.String("stream", "", "stdout or stderr only")
	since := flags.String("since", "", "only lines after this RFC3339 timestamp")
	if err := a.parse(flags, args); err != nil {
		return err
	}
	if flags.NArg() == 0 {
		return errs.New(errs.CodeInvalidRequest, "devman logs needs a service name")
	}

	query := client.LogQuery{Tail: *tail, Stream: *stream}
	if *since != "" {
		parsed, err := time.Parse(time.RFC3339, *since)
		if err != nil {
			return errs.New(errs.CodeInvalidRequest,
				"--since must be an RFC3339 timestamp, e.g. 2026-01-31T09:00:00Z")
		}
		query.Since = parsed
	}

	api, err := a.client()
	if err != nil {
		return err
	}
	record, err := a.resolveProject(api, *project)
	if err != nil {
		return err
	}
	name := flags.Arg(0)

	// A service with no capture would otherwise look like a silent service, so
	// say why the output is empty before showing nothing.
	if svc, svcErr := api.Service(record.ID, name); svcErr == nil {
		if svc.Status == dto.StatusRunning && svc.Observability.LogCapture == dto.LogCaptureDetached {
			fmt.Fprintf(a.Stderr,
				"note: %s was adopted after a daemon restart, so its output is not captured; restart it to resume capture\n",
				svc.Name)
		}
	}

	if !*follow && !*short {
		records, logErr := api.Logs(record.ID, name, query)
		if logErr != nil {
			return logErr
		}
		for _, record := range records {
			a.printLog(record)
		}
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err = api.StreamLogs(ctx, record.ID, name, *tail, func(record dto.LogRecord) error {
		a.printLog(record)
		return nil
	})
	if ctx.Err() != nil {
		// Ctrl-C is how following ends; it is not a failure.
		return nil
	}
	return err
}

func (a *App) printLog(record dto.LogRecord) {
	if a.JSON {
		a.printNDJSON(record)
		return
	}
	marker := ""
	if record.Stream == "stderr" {
		marker = " !"
	}
	a.printf("%s%s %s\n", record.Timestamp.Local().Format("15:04:05.000"), marker, record.Message)
}

// cmdEvents shows the daemon event log.
func (a *App) cmdEvents(args []string) error {
	flags := a.newFlags("events")
	follow := flags.Bool("follow", false, "stream events until interrupted")
	short := flags.Bool("f", false, "alias for --follow")
	limit := flags.Int("limit", 50, "how many recent events to show")
	if err := a.parse(flags, args); err != nil {
		return err
	}

	api, err := a.client()
	if err != nil {
		return err
	}

	if !*follow && !*short {
		events, eventErr := api.Events(*limit)
		if eventErr != nil {
			return eventErr
		}
		if a.JSON {
			a.printJSON(events)
			return nil
		}
		for _, event := range events {
			a.printEvent(event)
		}
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err = api.StreamEvents(ctx, *limit, func(event dto.Event) error {
		a.printEvent(event)
		return nil
	})
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (a *App) printEvent(event dto.Event) {
	if a.JSON {
		a.printNDJSON(event)
		return
	}
	subject := event.Project
	if event.Service != "" {
		subject += "/" + event.Service
	}
	line := fmt.Sprintf("%s  %-24s %s",
		event.Timestamp.Local().Format("15:04:05"), event.Type, subject)
	if event.Message != "" {
		line += "  " + event.Message
	}
	a.println(strings.TrimRight(line, " "))
}

// cmdPorts shows allocations, or answers who holds one port.
//
// The single-port form deliberately reports foreign processes too: "port 3000 is
// taken" is only useful if it also says by what.
func (a *App) cmdPorts(args []string) error {
	flags := a.newFlags("ports")
	if err := a.parse(flags, args); err != nil {
		return err
	}

	api, err := a.client()
	if err != nil {
		return err
	}

	if flags.NArg() > 0 {
		port, convErr := strconv.Atoi(flags.Arg(0))
		if convErr != nil || port <= 0 || port > 65535 {
			return errs.New(errs.CodeInvalidRequest, "%q is not a port number", flags.Arg(0))
		}
		usage, usageErr := api.PortUsage(port)
		if usageErr != nil {
			return usageErr
		}
		if a.JSON {
			a.printJSON(usage)
			return nil
		}
		a.printPortUsage(usage)
		return nil
	}

	allocations, err := api.Ports()
	if err != nil {
		return err
	}
	if a.JSON {
		a.printJSON(allocations)
		return nil
	}
	if len(allocations) == 0 {
		a.println("no ports allocated")
		return nil
	}
	sort.SliceStable(allocations, func(i, j int) bool { return allocations[i].Port < allocations[j].Port })
	table := newTable("PORT", "STATUS", "PROJECT", "SERVICE", "NAME", "ENV")
	for _, allocation := range allocations {
		table.add(
			strconv.Itoa(allocation.Port),
			string(allocation.Status),
			allocation.Project,
			allocation.Service,
			allocation.Name,
			allocation.EnvVar,
		)
	}
	table.render(a.Stdout)
	return nil
}

func (a *App) printPortUsage(usage dto.PortUsage) {
	if !usage.Occupied && usage.Allocation == nil {
		a.printf("%d is free\n", usage.Port)
		return
	}
	if usage.Allocation != nil {
		a.printf("%d  %s  %s/%s (%s)\n",
			usage.Port, usage.Allocation.Status,
			usage.Allocation.Project, usage.Allocation.Service, usage.Allocation.Name)
		return
	}
	owner := "another process"
	if usage.Owner != nil {
		switch {
		case usage.Owner.Name != "" && usage.Owner.PID != 0:
			owner = fmt.Sprintf("%s (pid %d)", usage.Owner.Name, usage.Owner.PID)
		case usage.Owner.Name != "":
			owner = usage.Owner.Name
		case usage.Owner.PID != 0:
			owner = fmt.Sprintf("pid %d", usage.Owner.PID)
		}
	}
	a.printf("%d is held by %s, which DevMan does not manage\n", usage.Port, owner)
}
