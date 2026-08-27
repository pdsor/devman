package cli

import (
	"flag"
	"os"
	"path/filepath"

	"github.com/devman-project/devman/pkg/config"
	"github.com/devman-project/devman/pkg/dto"
	"github.com/devman-project/devman/pkg/errs"
)

// These commands run entirely in this process. They are the ones a user needs
// before a daemon is involved at all: writing a configuration, checking it, and
// finding out where DevMan keeps its data.

func (a *App) newFlags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet("devman "+name, flag.ContinueOnError)
	flags.SetOutput(a.Stderr)
	return flags
}

func (a *App) cmdVersion(args []string) error {
	flags := a.newFlags("version")
	if err := a.parse(flags, args); err != nil {
		return err
	}
	if a.JSON {
		a.printJSON(map[string]string{
			"version":        a.Version,
			"api_version":    "v1",
			"schema_version": "1",
		})
		return nil
	}
	a.printf("devman %s (api v1, devman.yaml schema %d)\n", a.Version, config.SchemaVersion)
	return nil
}

func (a *App) cmdPaths(args []string) error {
	flags := a.newFlags("paths")
	if err := a.parse(flags, args); err != nil {
		return err
	}
	// Resolved locally on purpose: a user asking where the data lives is often
	// asking precisely because the daemon will not start.
	value := dto.Paths{
		Home:      a.Layout.Home,
		Settings:  a.Layout.Settings,
		Database:  a.Layout.Database,
		Daemon:    a.Layout.Daemon,
		AuthToken: a.Layout.AuthToken,
		Logs:      a.Layout.Logs,
	}
	if a.JSON {
		a.printJSON(value)
		return nil
	}
	table := newTable("WHAT", "PATH")
	table.add("data directory", value.Home)
	table.add("settings", value.Settings)
	table.add("database", value.Database)
	table.add("daemon record", value.Daemon)
	table.add("auth token", value.AuthToken)
	table.add("logs", value.Logs)
	table.render(a.Stdout)
	return nil
}

func (a *App) cmdValidate(args []string) error {
	flags := a.newFlags("validate")
	if err := a.parse(flags, args); err != nil {
		return err
	}

	target := "."
	if flags.NArg() > 0 {
		target = flags.Arg(0)
	}
	cfg, err := config.Load(target)
	if err != nil {
		return err
	}
	result := cfg.Validate(config.DefaultValidateOptions())

	if a.JSON {
		a.printJSON(result)
		if !result.Valid {
			return errs.New(errs.CodeConfigInvalid, "%s is not valid", cfg.ConfigPath)
		}
		return nil
	}

	a.printf("%s\n", cfg.ConfigPath)
	for _, issue := range result.Errors {
		a.printf("  error   %s: %s\n", issue.Path, issue.Message)
	}
	for _, issue := range result.Warnings {
		a.printf("  warning %s: %s\n", issue.Path, issue.Message)
	}
	if !result.Valid {
		return errs.New(errs.CodeConfigInvalid,
			"%d error(s) in %s", len(result.Errors), cfg.ConfigPath)
	}
	a.printf("  valid: %d service(s)\n", len(cfg.Services))
	return nil
}

// cmdInit writes a starter devman.yaml.
//
// The generated file always uses the canonical spelling of every field, because
// it is the example most users will copy from: `ports:` as a list, no `port:`
// sugar, and no health check invented for them. DevMan does not guess a probe.
func (a *App) cmdInit(args []string) error {
	flags := a.newFlags("init")
	force := flags.Bool("force", false, "overwrite an existing devman.yaml")
	name := flags.String("name", "", "project name (defaults to the directory name)")
	if err := a.parse(flags, args); err != nil {
		return err
	}

	root := "."
	if flags.NArg() > 0 {
		root = flags.Arg(0)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot resolve %s", root)
	}
	target := filepath.Join(absRoot, config.CanonicalFileName)
	if _, statErr := os.Stat(target); statErr == nil && !*force {
		return errs.New(errs.CodeProjectExists,
			"%s already exists; pass --force to replace it", target)
	}

	projectName := *name
	if projectName == "" {
		projectName = filepath.Base(absRoot)
	}
	body := starterConfig(projectName, absRoot)
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot write %s", target)
	}

	if a.JSON {
		a.printJSON(map[string]string{"config_path": target, "project": projectName})
		return nil
	}
	a.printf("wrote %s\n", target)
	a.println("next: review the commands, then run devman register .")
	return nil
}

// starterConfig builds the scaffold, filling in what can be detected and leaving
// the rest as commented guidance rather than a guess.
func starterConfig(name, root string) string {
	body := "version: 1\n\nproject:\n  name: " + name + "\n\nservices:\n"

	if hasFile(root, "package.json") {
		body += `  frontend:
    display_name: Frontend
    runtime: host
    cwd: .
    command: npm
    args: [run, dev]
    ports:
      - name: http
        value: auto
        preferred: 3000
        range: frontend
        env: PORT
    autostart: true
`
	} else {
		body += `  # A host service is a process DevMan owns. Replace this with yours.
  app:
    display_name: App
    runtime: host
    cwd: .
    command: echo
    args: ["replace me"]
    ports:
      - name: http
        value: auto
        env: PORT
`
	}

	body += `
# Add more services as needed. Useful fields:
#
#   depends_on:            wait for another service
#     database:
#       condition: healthy
#   health:                DevMan never guesses a probe; declare one to get
#     type: http           real health rather than "the process is alive"
#     url: http://127.0.0.1:${PORT}/health
#   restart:
#     policy: on-failure
#     max_attempts: 3
#   env_file: [.env, .env.local]
#   required_env: [DATABASE_URL]
#   platform:              per-OS differences go here, never in ` + "`command`" + `
#     windows:
#       command: npm.cmd
`
	return body
}

func hasFile(root, name string) bool {
	info, err := os.Stat(filepath.Join(root, name))
	return err == nil && !info.IsDir()
}
