//go:build integration

// Package compose holds the Docker Compose runtime integration suite.
//
// These tests are not unit tests with a mocked docker: they run a real
// `docker compose up`, look at real containers, and assert that what DevMan
// reports matches what Docker actually did. Compose has its own lifecycle,
// its own dependency and health semantics, and its own view of published ports,
// and every one of those is a place where DevMan can be confidently wrong.
//
// Run them with:
//
//	go test -tags=integration ./tests/integration/compose/...
//
// Without Docker they skip — unless DEVMAN_REQUIRE_DOCKER=1, in which case a
// missing Docker is a failure. CI sets it, because a Docker gate that silently
// skips is a gate that protects nothing.
package compose

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devman-project/devman/internal/testenv"
)

// This package's private daemon port window. Every suite gets its own because
// `go test ./...` runs packages in parallel.
var window = testenv.PortWindow{Start: 39700, End: 39749}

func TestMain(m *testing.M) { testenv.RunMain(m) }

// requireDocker gates the suite on a usable Docker engine.
//
// "Usable" means the daemon answers, not merely that the CLI is installed —
// those are different failures and DevMan is expected to tell them apart.
func requireDocker(t *testing.T) {
	t.Helper()

	required := os.Getenv("DEVMAN_REQUIRE_DOCKER") == "1"
	refuse := func(reason string) {
		if required {
			t.Fatalf("DEVMAN_REQUIRE_DOCKER=1 but %s", reason)
		}
		t.Skip(reason)
	}

	if _, err := exec.LookPath("docker"); err != nil {
		refuse("docker is not on PATH")
	}
	out, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput()
	if err != nil {
		refuse("the Docker daemon is not reachable: " + strings.TrimSpace(string(out)))
	}
}

// dockerVersions reports the engine and compose versions, for the verification
// report.
func dockerVersions(t *testing.T) (engine, compose string) {
	t.Helper()
	engineOut, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").Output()
	if err == nil {
		engine = strings.TrimSpace(string(engineOut))
	}
	composeOut, err := exec.Command("docker", "compose", "version", "--short").Output()
	if err == nil {
		compose = strings.TrimSpace(string(composeOut))
	}
	return engine, compose
}

// Fixture containers run an image the suite builds itself from
// tests/fixtures/httpserver. See buildFixtureImage for why there is no stock
// image here.
const fixtureImage = "devman-fixture:integration"

var (
	buildOnce sync.Once
	buildErr  error
)

// buildFixtureImage compiles the fixture server for the engine's platform and
// wraps it in a FROM scratch image.
//
// Nothing is pulled from a registry. A suite that depends on Docker Hub fails
// for reasons that have nothing to do with DevMan — rate limits, an unreachable
// mirror, a proxy the engine cannot use — and those failures are indistinguishable
// from a real regression in a CI log. Building a two megabyte static binary is
// faster than pulling anyway.
func buildFixtureImage(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() { buildErr = buildImage() })
	if buildErr != nil {
		t.Fatalf("cannot build the fixture image: %v", buildErr)
	}
	return fixtureImage
}

func buildImage() error {
	osType, arch, err := enginePlatform()
	if err != nil {
		return err
	}
	if osType != "linux" {
		return fmt.Errorf("the fixture image is Linux only, engine reports %q", osType)
	}

	root, err := repoRoot()
	if err != nil {
		return err
	}

	context, err := os.MkdirTemp("", "devman-fixture-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(context)

	build := exec.Command("go", "build", "-trimpath", "-o",
		filepath.Join(context, "server"), "./tests/fixtures/httpserver")
	build.Dir = root
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %w: %s", err, out)
	}

	dockerfile := "FROM scratch\nCOPY server /server\nENTRYPOINT [\"/server\"]\n"
	if err := os.WriteFile(filepath.Join(context, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return err
	}

	image := exec.Command("docker", "build", "-t", fixtureImage, ".")
	image.Dir = context
	if out, err := image.CombinedOutput(); err != nil {
		return fmt.Errorf("docker build: %w: %s", err, out)
	}
	return nil
}

// enginePlatform asks the engine what it runs, because a container built for the
// wrong architecture fails in a way that looks like a DevMan bug.
func enginePlatform() (osType, goarch string, err error) {
	out, err := exec.Command("docker", "info", "--format", "{{.OSType}} {{.Architecture}}").Output()
	if err != nil {
		return "", "", fmt.Errorf("docker info: %w", err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) != 2 {
		return "", "", fmt.Errorf("unexpected docker info output %q", out)
	}
	switch fields[1] {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "aarch64", "arm64":
		goarch = "arm64"
	default:
		return "", "", fmt.Errorf("unsupported engine architecture %q", fields[1])
	}
	return fields[0], goarch, nil
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}



// project is one compose fixture on disk plus the compose project name DevMan
// will use, so the test can address exactly the same containers DevMan does.
type project struct {
	t    *testing.T
	root string
	name string
	file string
}

// newProject writes a fixture and guarantees its containers are removed
// afterwards, including volumes: an integration suite that leaks containers
// poisons the next run.
func newProject(t *testing.T, name string, files map[string]string) *project {
	t.Helper()

	unique := fmt.Sprintf("devman-it-%s-%d", name, os.Getpid())
	image := buildFixtureImage(t)
	rendered := map[string]string{}
	for path, body := range files {
		content := strings.ReplaceAll(body, "%PROJECT%", unique)
		content = strings.ReplaceAll(content, "%IMAGE%", image)
		rendered[path] = content
	}
	root := testenv.WriteProjectFiles(t, rendered)

	p := &project{t: t, root: root, name: unique, file: "compose.yaml"}
	t.Cleanup(func() {
		out, err := p.compose("down", "-v", "--remove-orphans", "--timeout", "5")
		if err != nil {
			t.Logf("compose cleanup failed: %v\n%s", err, out)
		}
	})
	return p
}

// compose runs a docker compose command against the fixture.
//
// PORT is given a harmless default because the fixture interpolates it: the
// test's own compose calls only need the file to parse, while the value that
// matters is the one DevMan injects when it starts the service.
func (p *project) compose(args ...string) (string, error) {
	p.t.Helper()
	full := append([]string{"compose", "-f", p.file, "-p", p.name}, args...)
	cmd := exec.Command("docker", full...)
	cmd.Dir = p.root
	cmd.Env = append(os.Environ(), "PORT=0")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// container is the part of `docker compose ps --format json` this suite asserts on.
type container struct {
	Name       string `json:"Name"`
	Service    string `json:"Service"`
	State      string `json:"State"`
	ExitCode   int    `json:"ExitCode"`
	Publishers []struct {
		URL           string `json:"URL"`
		TargetPort    int    `json:"TargetPort"`
		PublishedPort int    `json:"PublishedPort"`
		Protocol      string `json:"Protocol"`
	} `json:"Publishers"`
}

// containers asks Compose for the truth, including stopped containers.
//
// Compose has emitted `--format json` as both a JSON array and one object per
// line depending on version, so both are accepted rather than pinning the suite
// to one Compose release.
func (p *project) containers() []container {
	p.t.Helper()
	out, err := p.compose("ps", "-a", "--format", "json")
	if err != nil {
		p.t.Fatalf("docker compose ps failed: %v\n%s", err, out)
	}
	body := strings.TrimSpace(out)
	if body == "" {
		return nil
	}

	var asArray []container
	if err := json.Unmarshal([]byte(body), &asArray); err == nil {
		return asArray
	}

	var lines []container
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var one container
		if err := json.Unmarshal([]byte(line), &one); err != nil {
			p.t.Fatalf("cannot decode compose ps output line %q: %v", line, err)
		}
		lines = append(lines, one)
	}
	return lines
}

// container finds one service's container, or nil when Compose does not know it.
func (p *project) container(service string) *container {
	p.t.Helper()
	for _, found := range p.containers() {
		if found.Service == service {
			return &found
		}
	}
	return nil
}

// publishedPort is the host port Docker actually published for a container port.
func (p *project) publishedPort(service string, target int) int {
	p.t.Helper()
	found := p.container(service)
	if found == nil {
		p.t.Fatalf("compose does not know a container for service %q", service)
	}
	for _, publisher := range found.Publishers {
		if publisher.TargetPort == target && publisher.PublishedPort != 0 {
			return publisher.PublishedPort
		}
	}
	p.t.Fatalf("container %s publishes nothing for port %d: %+v",
		found.Name, target, found.Publishers)
	return 0
}

// volumeExists reports whether a named volume is still present, which is how
// "stop must not destroy data" is asserted.
func volumeExists(t *testing.T, name string) bool {
	t.Helper()
	out, err := exec.Command("docker", "volume", "ls", "--format", "{{.Name}}").Output()
	if err != nil {
		t.Fatalf("docker volume ls failed: %v", err)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.TrimSpace(line) == name {
			return true
		}
	}
	return false
}

// waitForState polls Compose until a service reaches a state.
func (p *project) waitForState(service, state string, timeout time.Duration) {
	p.t.Helper()
	testenv.WaitFor(p.t, fmt.Sprintf("compose service %s to be %s", service, state), timeout, func() bool {
		found := p.container(service)
		return found != nil && found.State == state
	})
}

// inspect reads one Go-template field out of `docker inspect` for a container.
//
// Compose's ps output does not carry the container healthcheck result or the
// exact start time, and both are needed to prove that Compose's own dependency
// and health semantics were honoured rather than approximated.
func inspect(t *testing.T, container, format string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "--format", format, container).CombinedOutput()
	if err != nil {
		t.Fatalf("docker inspect %s %s failed: %v\n%s", container, format, err, out)
	}
	return strings.TrimSpace(string(out))
}

// containerHealth is the container's own healthcheck verdict: starting, healthy
// or unhealthy. Empty when the image declares no healthcheck.
func (p *project) containerHealth(service string) string {
	p.t.Helper()
	found := p.container(service)
	if found == nil {
		p.t.Fatalf("compose does not know a container for service %q", service)
	}
	status := inspect(p.t, found.Name, "{{if .State.Health}}{{.State.Health.Status}}{{end}}")
	return status
}

// startedAt is when Docker started the container, used to assert ordering.
func (p *project) startedAt(service string) time.Time {
	p.t.Helper()
	found := p.container(service)
	if found == nil {
		p.t.Fatalf("compose does not know a container for service %q", service)
	}
	raw := inspect(p.t, found.Name, "{{.State.StartedAt}}")
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		p.t.Fatalf("cannot parse StartedAt %q: %v", raw, err)
	}
	return parsed
}

