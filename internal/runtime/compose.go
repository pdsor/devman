package runtime

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/pkg/config"
	"github.com/devman-project/devman/pkg/errs"
)

// ComposeRuntime manages one service of a docker-compose file.
//
// DevMan does not reimplement compose: it calls `docker compose up -d` for the
// single declared service and then follows its logs. The log follower is also
// how liveness is observed, because `docker compose logs -f` exits when the
// container stops.
type ComposeRuntime struct{}

// Kind implements Runtime.
func (ComposeRuntime) Kind() config.RuntimeKind { return config.RuntimeDockerCompose }

// Start brings the compose service up.
//
// A missing Docker installation is reported as DOCKER_NOT_FOUND, which the
// supervisor turns into BLOCKED rather than FAILED: nothing is broken in the
// project, a prerequisite is simply absent.
func (ComposeRuntime) Start(req StartRequest) (Handle, error) {
	docker, err := lookPath("docker")
	if err != nil {
		return nil, errs.New(errs.CodeDockerNotFound,
			"docker was not found on PATH, so service %s cannot be started", req.Service).
			With("service", req.Service)
	}

	target := composeTarget(req)
	base := composeArgs(req.Compose, req.Dir)

	upArgs := append(append([]string{}, base...), "up", "-d", target)
	if err := runSync(docker, upArgs, req, 0); err != nil {
		return nil, err
	}

	// The follower is a tracked process so its output flows into the normal log
	// pipeline and its exit signals that the container is gone.
	follower, err := platform.Spawn(platform.SpawnRequest{
		Command: docker,
		Args:    append(append([]string{}, base...), "logs", "-f", "--tail", "0", target),
		Dir:     req.Dir,
		Env:     req.Env,
		Stdout:  req.Stdout,
		Stderr:  req.Stderr,
	})
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "cannot follow logs of %s", req.Service)
	}

	return &composeHandle{
		docker: docker,
		base:   base,
		target: target,
		dir:    req.Dir,
		env:    req.Env,
		follow: follower,
		line:   CommandLine("docker", append(append([]string{}, base...), "up", "-d", target)),
	}, nil
}

// composeTarget is the compose service name, defaulting to the DevMan service
// name when `compose.service` is not declared.
func composeTarget(req StartRequest) string {
	if req.Compose != nil && req.Compose.Service != "" {
		return req.Compose.Service
	}
	return req.Service
}

// composeArgs builds the `compose -f ... -p ...` prefix shared by every call,
// so up, stop and logs always address the same project.
func composeArgs(spec *config.ComposeSpec, dir string) []string {
	args := []string{"compose"}
	if spec == nil {
		return args
	}
	if spec.File != "" {
		file := spec.File
		if !filepath.IsAbs(file) {
			file = filepath.Join(dir, file)
		}
		args = append(args, "-f", file)
	}
	if spec.Project != "" {
		args = append(args, "-p", spec.Project)
	}
	return args
}

// runSync runs a docker command to completion, streaming its output into the
// service log so a compose failure is visible where users already look.
func runSync(docker string, args []string, req StartRequest, timeout time.Duration) error {
	proc, err := platform.Spawn(platform.SpawnRequest{
		Command: docker,
		Args:    args,
		Dir:     req.Dir,
		Env:     req.Env,
		Stdout:  req.Stdout,
		Stderr:  req.Stderr,
	})
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot run docker %s", strings.Join(args, " "))
	}
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	select {
	case <-proc.Done():
	case <-time.After(timeout):
		_ = proc.KillTree()
		return errs.New(errs.CodeTimeout, "docker %s did not finish within %s",
			strings.Join(args, " "), timeout)
	}
	status, _ := proc.Exit()
	if status.Code != 0 {
		return errs.New(errs.CodeInternal, "docker %s exited with code %d",
			strings.Join(args, " "), status.Code).With("exit_code", status.Code)
	}
	return nil
}

// composeHandle supervises a compose service through its log follower.
type composeHandle struct {
	docker string
	base   []string
	target string
	dir    string
	env    []string
	follow *platform.Process
	line   string
}

// PID is the follower's pid, not the container's. It is reported so the
// process tree is inspectable, and the supervisor never treats it as the
// service itself.
func (h *composeHandle) PID() int                    { return h.follow.PID() }
func (h *composeHandle) Identity() platform.Identity { return h.follow.Identity() }
func (h *composeHandle) CommandLine() string         { return h.line }
func (h *composeHandle) Running() bool               { return h.follow.Running() }
func (h *composeHandle) Done() <-chan struct{}       { return h.follow.Done() }

func (h *composeHandle) Exit() (platform.ExitStatus, error) { return h.follow.Exit() }

// Stop asks compose to stop the container, then tears down the follower.
func (h *composeHandle) Stop(graceful time.Duration) platform.StopOutcome {
	seconds := int(graceful.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	args := append(append([]string{}, h.base...), "stop", "-t", strconv.Itoa(seconds), h.target)

	outcome := platform.StopOutcome{}
	proc, err := platform.Spawn(platform.SpawnRequest{
		Command: h.docker,
		Args:    args,
		Dir:     h.dir,
		Env:     h.env,
	})
	if err != nil {
		outcome.SignalError = fmt.Sprintf("cannot run docker compose stop: %v", err)
	} else {
		outcome.SignalSent = true
		select {
		case <-proc.Done():
			status, _ := proc.Exit()
			outcome.Graceful = status.Code == 0
		case <-time.After(graceful + 30*time.Second):
			_ = proc.KillTree()
		}
	}

	// The follower exits by itself once the container is gone; stopping it is
	// both a fallback and how a forced stop is expressed.
	followOutcome := h.follow.Stop(5 * time.Second)
	outcome.Status = followOutcome.Status
	return outcome
}
