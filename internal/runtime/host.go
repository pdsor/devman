package runtime

import (
	"runtime"
	"strings"

	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/pkg/config"
	"github.com/devman-project/devman/pkg/errs"
)

// HostRuntime spawns a process on the machine DevMan runs on.
type HostRuntime struct{}

// Kind implements Runtime.
func (HostRuntime) Kind() config.RuntimeKind { return config.RuntimeHost }

// Start launches the service inside a kill-safe container: a Job Object on
// Windows, a process group on Unix. The whole tree therefore dies with the
// service, which is the single most important guarantee DevMan makes.
func (HostRuntime) Start(req StartRequest) (Handle, error) {
	if strings.TrimSpace(req.Command) == "" {
		return nil, errs.New(errs.CodeConfigInvalid, "service %s has no command", req.Service)
	}

	command, args, err := wrapShell(req.Command, req.Args, req.Shell)
	if err != nil {
		return nil, err
	}

	proc, err := platform.Spawn(platform.SpawnRequest{
		Command: command,
		Args:    args,
		Dir:     req.Dir,
		Env:     req.Env,
		Stdout:  req.Stdout,
		Stderr:  req.Stderr,
	})
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "cannot start %s", req.Service)
	}
	return &processHandle{proc: proc, line: CommandLine(req.Command, req.Args)}, nil
}

// wrapShell turns a service definition into the argv actually executed.
//
// `shell: true` means "the command is a shell line", so it is handed to the
// platform interpreter verbatim rather than being split by DevMan. Splitting a
// shell line ourselves would silently change the meaning of quotes, pipes and
// redirections.
func wrapShell(command string, args []string, shell config.ShellSpec) (string, []string, error) {
	if !shell.Enabled {
		return command, args, nil
	}
	// `shell: true` together with `args` is rejected by validation, so a line is
	// all there is to run.
	line := command
	if len(args) > 0 {
		line = CommandLine(command, args)
	}

	switch shell.Type {
	case config.ShellPowerShell:
		interpreter, err := lookPath(powershellName())
		if err != nil {
			return "", nil, err
		}
		return interpreter, []string{"-NoProfile", "-NonInteractive", "-Command", line}, nil
	case config.ShellDefault:
		if runtime.GOOS == "windows" {
			interpreter, err := lookPath("cmd.exe")
			if err != nil {
				return "", nil, err
			}
			// /D skips AutoRun, /S keeps the line intact, /C runs and exits.
			return interpreter, []string{"/D", "/S", "/C", line}, nil
		}
		return "/bin/sh", []string{"-c", line}, nil
	default:
		return "", nil, errs.New(errs.CodeConfigInvalid, "unknown shell type %q", shell.Type)
	}
}

// powershellName prefers PowerShell 7 when present and falls back to the
// built-in Windows PowerShell.
func powershellName() string {
	if runtime.GOOS == "windows" {
		if _, err := lookPath("pwsh.exe"); err == nil {
			return "pwsh.exe"
		}
		return "powershell.exe"
	}
	return "pwsh"
}
