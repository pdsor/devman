package cli

import (
	"bufio"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/devman-project/devman/internal/registry"
	"github.com/devman-project/devman/pkg/dto"
	"github.com/devman-project/devman/pkg/errs"
)

// cmdRegister registers a project after showing exactly what it will be allowed
// to run.
//
// The confirmation is the security boundary of DevMan: a devman.yaml is code, and
// registering it hands DevMan permission to execute that code. `--trust` exists
// for non-interactive use, and it is deliberately *not* permanent trust of the
// path: what gets stored is the project's execution fingerprint, so any later
// change to a command, cwd, shell, env or compose target requires approval
// again.
func (a *App) cmdRegister(args []string) error {
	flags := a.newFlags("register")
	trust := flags.Bool("trust", false, "approve the commands without prompting")
	yes := flags.Bool("yes", false, "alias for --trust")
	if err := a.parse(flags, args); err != nil {
		return err
	}

	target := "."
	if flags.NArg() > 0 {
		target = flags.Arg(0)
	}
	absolute, err := filepath.Abs(target)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot resolve %s", target)
	}

	api, err := a.client()
	if err != nil {
		return err
	}
	preview, err := api.Inspect(absolute)
	if err != nil {
		return err
	}

	approved := *trust || *yes
	if a.JSON && !approved {
		// A non-interactive caller must never be prompted, and must never have
		// trust granted implicitly on its behalf.
		a.printJSON(preview)
		return errs.New(errs.CodeProjectUntrusted,
			"registering %s requires explicit approval: re-run with --trust after reviewing the commands",
			preview.Name)
	}
	if !approved {
		a.printPreview(preview)
		confirmed, promptErr := a.confirm("Allow DevMan to run these commands?")
		if promptErr != nil {
			return promptErr
		}
		if !confirmed {
			return errs.New(errs.CodeProjectUntrusted, "registration cancelled")
		}
		approved = true
	}

	project, err := api.Register(absolute, approved)
	if err != nil {
		return err
	}
	if a.JSON {
		a.printJSON(project)
		return nil
	}
	a.printf("registered %s (%s)\n", project.Name, project.ID)
	a.printf("  config: %s\n", project.ConfigPath)
	a.println("next: devman start")
	return nil
}

// printPreview renders the four things a user needs in order to decide: what the
// project is, what it will execute, where, and which files it reads.
func (a *App) printPreview(preview registry.Preview) {
	a.printf("Project:      %s\n", preview.Name)
	a.printf("Location:     %s\n", preview.Path)
	a.printf("Config:       %s\n", preview.ConfigPath)
	if preview.AlreadyRegistered {
		a.println("Status:       already registered; the commands below changed")
	}
	a.println()

	for _, summary := range preview.Execution {
		a.printf("  %s (%s)\n", summary.Service, summary.Runtime)
		if summary.CommandLine != "" {
			a.printf("    command:  %s\n", summary.CommandLine)
		}
		if summary.Shell != "" {
			a.printf("    shell:    %s\n", summary.Shell)
		}
		a.printf("    cwd:      %s\n", summary.CWD)
		if len(summary.EnvFiles) > 0 {
			a.printf("    env_file: %s\n", strings.Join(summary.EnvFiles, ", "))
		}
		if summary.Compose != "" {
			a.printf("    docker:   %s\n", summary.Compose)
		}
	}

	if preview.Validation != nil {
		for _, issue := range preview.Validation.Errors {
			a.printf("\n  error   %s: %s\n", issue.Path, issue.Message)
		}
		for _, issue := range preview.Validation.Warnings {
			a.printf("  warning %s: %s\n", issue.Path, issue.Message)
		}
	}
	a.println()
}

// confirm asks a yes/no question on the terminal.
func (a *App) confirm(question string) (bool, error) {
	fmt.Fprintf(a.Stdout, "%s [y/N] ", question)
	reader := bufio.NewReader(a.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return false, errs.New(errs.CodeInvalidRequest,
			"cannot read a confirmation; use --trust for non-interactive use")
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// cmdTrust approves the project's current execution fingerprint.
func (a *App) cmdTrust(args []string) error {
	flags := a.newFlags("trust")
	revoke := flags.Bool("revoke", false, "withdraw approval")
	force := flags.Bool("yes", false, "approve without prompting")
	selector := flags.String("project", "", "project id, name or path")
	if err := a.parse(flags, args); err != nil {
		return err
	}

	api, err := a.client()
	if err != nil {
		return err
	}
	project, err := a.resolveProject(api, selectorFrom(*selector, flags.Arg(0)))
	if err != nil {
		return err
	}

	if !*revoke && !*force && !a.JSON {
		preview, inspectErr := api.Inspect(project.Path)
		if inspectErr != nil {
			return inspectErr
		}
		a.printPreview(preview)
		confirmed, promptErr := a.confirm("Approve these commands?")
		if promptErr != nil {
			return promptErr
		}
		if !confirmed {
			return errs.New(errs.CodeProjectUntrusted, "not approved")
		}
	}

	updated, err := api.Trust(project.ID, *revoke)
	if err != nil {
		return err
	}
	if a.JSON {
		a.printJSON(updated)
		return nil
	}
	if *revoke {
		a.printf("revoked approval for %s\n", updated.Name)
	} else {
		a.printf("approved %s\n", updated.Name)
	}
	return nil
}

func (a *App) cmdUnregister(args []string) error {
	flags := a.newFlags("unregister")
	selector := flags.String("project", "", "project id, name or path")
	if err := a.parse(flags, args); err != nil {
		return err
	}
	api, err := a.client()
	if err != nil {
		return err
	}
	project, err := a.resolveProject(api, selectorFrom(*selector, flags.Arg(0)))
	if err != nil {
		return err
	}
	if err := api.Unregister(project.ID); err != nil {
		return err
	}
	if a.JSON {
		a.printJSON(map[string]string{"unregistered": project.ID})
		return nil
	}
	a.printf("unregistered %s\n", project.Name)
	return nil
}

func (a *App) cmdList(args []string) error {
	flags := a.newFlags("list")
	if err := a.parse(flags, args); err != nil {
		return err
	}
	api, err := a.client()
	if err != nil {
		return err
	}
	projects, err := api.Projects(false)
	if err != nil {
		return err
	}
	if a.JSON {
		a.printJSON(projects)
		return nil
	}
	if len(projects) == 0 {
		a.println("no projects registered; run devman register . in a project directory")
		return nil
	}
	table := newTable("PROJECT", "STATUS", "SERVICES", "TRUST", "PATH")
	for _, project := range projects {
		name := project.Name
		if project.DisplayName != "" {
			name = project.DisplayName
		}
		trust := "approved"
		if !project.Trusted {
			trust = "needs approval"
		}
		services := fmt.Sprintf("%d/%d running", project.Summary.Running, project.Summary.Total)
		if project.ConfigError != nil {
			services = "config error"
		}
		table.add(name, string(project.Status), services, trust, project.Path)
	}
	table.render(a.Stdout)
	return nil
}

func (a *App) cmdStatus(args []string) error {
	flags := a.newFlags("status")
	all := flags.Bool("all", false, "show every registered project")
	project := flags.String("project", "", "project id, name or path")
	if err := a.parse(flags, args); err != nil {
		return err
	}
	api, err := a.client()
	if err != nil {
		return err
	}

	if *all {
		projects, listErr := api.Projects(true)
		if listErr != nil {
			return listErr
		}
		if a.JSON {
			a.printJSON(projects)
			return nil
		}
		for i, project := range projects {
			if i > 0 {
				a.println()
			}
			a.printProject(project)
		}
		return nil
	}

	record, err := a.resolveProject(api, selectorFrom(*project, flags.Arg(0)))
	if err != nil {
		return err
	}
	if a.JSON {
		a.printJSON(record)
		return nil
	}
	a.printProject(record)
	return nil
}

// selectorFrom prefers an explicit --project over a positional selector, so both
// spellings work and neither silently wins over the other.
func selectorFrom(flagValue, positional string) string {
	if flagValue != "" {
		return flagValue
	}
	return positional
}

func (a *App) printProject(project dto.Project) {
	name := project.Name
	if project.DisplayName != "" {
		name = project.DisplayName
	}
	a.printf("%s  %s\n", name, project.Status)
	a.printf("%s\n", project.Path)
	if !project.Trusted {
		a.println("this project needs approval before it can start: devman trust")
	}
	if project.ConfigError != nil {
		a.printf("config error: %s\n", project.ConfigError.Message)
		return
	}
	a.println()
	a.printServices(project.Services)
}

// cmdOpen opens the GUI, or a service's HTTP port in the browser.
//
// Service matching is scoped to one project: a fuzzy search across every
// registered project would eventually open the wrong thing, so an unknown name
// is an error rather than a guess.
func (a *App) cmdOpen(args []string) error {
	flags := a.newFlags("open")
	project := flags.String("project", "", "project id, name or path")
	portName := flags.String("port", "", "which declared port to open")
	if err := a.parse(flags, args); err != nil {
		return err
	}

	api, err := a.client()
	if err != nil {
		return err
	}
	if flags.NArg() == 0 {
		// No service named: the GUI is the thing to open. Until it ships, the
		// daemon address is the honest answer rather than a fabricated URL.
		status, statusErr := api.DaemonStatus()
		if statusErr != nil {
			return statusErr
		}
		address := fmt.Sprintf("http://%s:%d", status.Info.Host, status.Info.Port)
		if a.JSON {
			a.printJSON(map[string]string{"daemon": address})
			return nil
		}
		a.printf("the DevMan GUI is not part of this build; the daemon API is at %s\n", address)
		return nil
	}

	record, err := a.resolveProject(api, *project)
	if err != nil {
		return err
	}
	service, err := api.Service(record.ID, flags.Arg(0))
	if err != nil {
		return err
	}

	url, err := serviceURL(service, *portName)
	if err != nil {
		return err
	}
	if a.JSON {
		a.printJSON(map[string]string{"url": url})
		return nil
	}
	if err := openBrowser(url); err != nil {
		a.printf("%s\n", url)
		return nil
	}
	a.printf("opened %s\n", url)
	return nil
}

// serviceURL picks the port to open, preferring the one named "http" and
// requiring an explicit choice when there is no obvious default.
func serviceURL(service dto.Service, portName string) (string, error) {
	if len(service.Ports) == 0 {
		return "", errs.New(errs.CodeInvalidRequest,
			"%s declares no ports, so there is nothing to open", service.Name)
	}
	if portName != "" {
		for _, port := range service.Ports {
			if port.Name == portName {
				return fmt.Sprintf("http://127.0.0.1:%d", port.Port), nil
			}
		}
		return "", errs.New(errs.CodeInvalidRequest,
			"%s has no port named %q", service.Name, portName)
	}
	if service.URL != "" {
		return service.URL, nil
	}
	for _, port := range service.Ports {
		if port.Name == "http" {
			return fmt.Sprintf("http://127.0.0.1:%d", port.Port), nil
		}
	}
	if len(service.Ports) == 1 {
		return fmt.Sprintf("http://127.0.0.1:%d", service.Ports[0].Port), nil
	}
	names := make([]string, 0, len(service.Ports))
	for _, port := range service.Ports {
		names = append(names, port.Name)
	}
	return "", errs.New(errs.CodeInvalidRequest,
		"%s declares several ports (%s); choose one with --port",
		service.Name, strings.Join(names, ", "))
}

// openBrowser hands a URL to the desktop.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
