// Command uifixture serves the daemon API with canned data so the desktop shell
// can be looked at, screenshotted and reviewed without a real project on the
// machine.
//
// It exists because GUI review kept depending on whatever happened to be running
// locally: the interesting states — UNHEALTHY while RUNNING, CRASHED with an exit
// code, BLOCKED on a missing Docker engine, a port in CONFLICT, a CJK display
// name long enough to overflow a column — are exactly the ones that are awkward
// to reproduce on demand, so they were never reviewed at all.
//
// The fixture speaks HTTP rather than patching the frontend. The GUI's only door
// to the daemon is internal/client's protocol, and a mock inside the app would
// bypass the very code that decodes the error envelope. Everything here is a real
// response over a real socket.
//
// Usage:
//
//	go run ./tools/uifixture                 # serve on 127.0.0.1:39190
//	go run ./tools/uifixture -scenario empty # the first-run empty state
//
// It prints both ways in: a DEVMAN_HOME for the desktop shell, which discovers
// the daemon exactly as it would a real one, and the address plus token to paste
// into the browser build. The frontend also reads VITE_DEVMAN_UI_TEST, which
// skips that paste step entirely.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/devman-project/devman/internal/client"
	"github.com/devman-project/devman/internal/paths"
	"github.com/devman-project/devman/internal/registry"
	"github.com/devman-project/devman/pkg/config"
	"github.com/devman-project/devman/pkg/dto"
	"github.com/devman-project/devman/pkg/errs"
)

// Token is fixed rather than random: the point of the fixture is a repeatable
// session, and this daemon serves invented data on the loopback interface only.
const Token = "devman-ui-test"

func main() {
	port := flag.Int("port", 39190, "port to serve on")
	home := flag.String("home", "", "data directory to publish daemon.json in (default: a temp directory)")
	scenario := flag.String("scenario", "rich", "rich | empty")
	flag.Parse()

	if *home == "" {
		*home = filepath.Join(os.TempDir(), "devman-uitest")
	}
	layout := paths.For(*home)
	if err := layout.EnsureDirs(); err != nil {
		fail(err)
	}

	world := newWorld(*scenario, layout, *port)

	if err := publish(layout, *port); err != nil {
		fail(err)
	}
	defer func() {
		// A fixture that leaves a discovery record behind would make the next
		// real `devman` command talk to a port nobody is listening on.
		_ = os.Remove(layout.Daemon)
	}()

	mux := http.NewServeMux()
	world.routes(mux)

	address := fmt.Sprintf("127.0.0.1:%d", *port)
	fmt.Printf("DevMan UI fixture (%s scenario) on http://%s/api/v1\n", *scenario, address)
	fmt.Printf("  desktop shell:  set DEVMAN_HOME=%s, then start the app\n", *home)
	fmt.Printf("  browser build:  address http://%s/api/v1  token %s\n", address, Token)
	fmt.Printf("  or:             VITE_DEVMAN_UI_TEST=1 pnpm dev  (auto-connects here)\n")

	server := &http.Server{
		Addr:              address,
		Handler:           authenticated(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "uifixture: %v\n", err)
	os.Exit(1)
}

// publish writes daemon.json and the token file so the desktop shell's discovery
// path finds this process the same way it finds a real daemon.
func publish(layout paths.Layout, port int) error {
	info := dto.DaemonInfo{
		PID:             os.Getpid(),
		Port:            port,
		Host:            "127.0.0.1",
		StartedAt:       time.Now().UTC(),
		APIVersion:      "v1",
		Version:         "ui-fixture",
		GracefulSignals: true,
	}
	body, err := json.Marshal(info)
	if err != nil {
		return err
	}
	if err := os.WriteFile(layout.Daemon, body, 0o600); err != nil {
		return err
	}
	return paths.WriteSecret(layout.AuthToken, []byte(Token))

}

// authenticated mirrors the real daemon: a bearer token on requests, and the
// same token as a query parameter for the two SSE endpoints, because EventSource
// cannot set headers.
func authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if supplied == "" {
			supplied = r.URL.Query().Get("token")
		}
		if supplied != Token {
			writeError(w, http.StatusUnauthorized,
				errs.New(errs.CodeUnauthorized, "the auth token does not match"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err *errs.Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": string(err.Code), "message": err.Message, "path": err.Path,
	}})
}

// world holds the canned state. Mutating endpoints edit it, so clicking start or
// stop in the GUI changes what the next poll returns instead of doing nothing.
type world struct {
	layout   paths.Layout
	port     int
	started  time.Time
	projects []dto.Project
	ports    []dto.PortAllocation
	events   []dto.Event
	logs     []dto.LogRecord
	settings map[string]string
}

func (w *world) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/daemon/status", w.daemonStatus)
	mux.HandleFunc("POST /api/v1/daemon/shutdown", func(rw http.ResponseWriter, _ *http.Request) {
		writeJSON(rw, dto.OperationResult{})
	})
	mux.HandleFunc("GET /api/v1/paths", w.pathsHandler)
	mux.HandleFunc("GET /api/v1/settings", func(rw http.ResponseWriter, _ *http.Request) {
		writeJSON(rw, w.settings)
	})
	mux.HandleFunc("PUT /api/v1/settings", w.setSetting)
	mux.HandleFunc("GET /api/v1/tools", w.tools)

	mux.HandleFunc("GET /api/v1/projects", w.listProjects)
	mux.HandleFunc("POST /api/v1/projects", w.register)
	mux.HandleFunc("POST /api/v1/projects/inspect", w.inspect)
	mux.HandleFunc("GET /api/v1/projects/{id}", w.project)
	mux.HandleFunc("DELETE /api/v1/projects/{id}", func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/projects/{id}/trust", w.trust)
	mux.HandleFunc("GET /api/v1/projects/{id}/validate", w.validate)
	mux.HandleFunc("GET /api/v1/projects/{id}/config", w.configFile)
	mux.HandleFunc("PUT /api/v1/projects/{id}/config", w.configFile)
	mux.HandleFunc("GET /api/v1/projects/{id}/services", w.services)
	mux.HandleFunc("POST /api/v1/projects/{id}/start", w.projectDesire(dto.StatusRunning))
	mux.HandleFunc("POST /api/v1/projects/{id}/stop", w.projectDesire(dto.StatusStopped))
	mux.HandleFunc("POST /api/v1/projects/{id}/restart", w.projectDesire(dto.StatusRunning))
	mux.HandleFunc("POST /api/v1/projects/{id}/services/{name}/start", w.serviceDesire(dto.StatusRunning))
	mux.HandleFunc("POST /api/v1/projects/{id}/services/{name}/stop", w.serviceDesire(dto.StatusStopped))
	mux.HandleFunc("POST /api/v1/projects/{id}/services/{name}/restart", w.serviceDesire(dto.StatusRunning))
	mux.HandleFunc("GET /api/v1/projects/{id}/services/{name}/logs", w.serviceLogs)
	mux.HandleFunc("GET /api/v1/projects/{id}/services/{name}/logs/stream", w.logStream)

	mux.HandleFunc("GET /api/v1/ports", func(rw http.ResponseWriter, _ *http.Request) {
		writeJSON(rw, w.ports)
	})
	mux.HandleFunc("GET /api/v1/ports/{port}", w.portUsage)
	mux.HandleFunc("GET /api/v1/events", func(rw http.ResponseWriter, _ *http.Request) {
		writeJSON(rw, w.events)
	})
	mux.HandleFunc("GET /api/v1/events/stream", w.eventStream)
}

func (w *world) daemonStatus(rw http.ResponseWriter, _ *http.Request) {
	running := 0
	for _, project := range w.projects {
		running += project.Summary.Running
	}
	writeJSON(rw, dto.DaemonStatus{
		Info: dto.DaemonInfo{
			PID: os.Getpid(), Port: w.port, Host: "127.0.0.1",
			StartedAt: w.started, APIVersion: "v1", Version: "ui-fixture",
			GracefulSignals: true,
		},
		Uptime:   int64(time.Since(w.started).Seconds()),
		Projects: len(w.projects),
		Running:  running,
		DataDir:  w.layout.Home,
		LogsDir:  w.layout.Logs,
		Healthy:  true,
	})
}

func (w *world) pathsHandler(rw http.ResponseWriter, _ *http.Request) {
	writeJSON(rw, dto.Paths{
		Home: w.layout.Home, Settings: w.layout.Settings, Database: w.layout.Database,
		Daemon: w.layout.Daemon, AuthToken: w.layout.AuthToken, Logs: w.layout.Logs,
	})
}

func (w *world) setSetting(rw http.ResponseWriter, r *http.Request) {
	var body struct{ Key, Value string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(rw, http.StatusBadRequest, errs.New(errs.CodeInvalidRequest, "malformed body"))

		return
	}
	w.settings[body.Key] = body.Value
	writeJSON(rw, w.settings)
}

// tools deliberately includes found and missing entries: the Environment page
// exists because a GUI launched from the Start menu can have a PATH that never
// saw the user's shell profile, and that page is only worth reviewing with a
// missing tool on it.
func (w *world) tools(rw http.ResponseWriter, _ *http.Request) {
	writeJSON(rw, []dto.ToolResolution{
		{Name: "node", Path: `C:\Program Files\nodejs\node.exe`, Found: true},
		{Name: "pnpm", Path: `C:\Users\dev\AppData\Roaming\npm\pnpm.cmd`, Found: true},
		{Name: "python", Path: `C:\Python312\python.exe`, Found: true},
		{Name: "docker", Path: `C:\Program Files\Docker\Docker\resources\bin\docker.exe`, Found: true},
		{Name: "go", Path: `C:\Program Files\Go\bin\go.exe`, Found: true},
		{Name: "poetry", Found: false},
		{Name: "bun", Found: false},
	})
}

func (w *world) listProjects(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("services") == "true" {
		writeJSON(rw, w.projects)
		return
	}
	lean := make([]dto.Project, 0, len(w.projects))
	for _, project := range w.projects {
		copied := project
		copied.Services = nil
		lean = append(lean, copied)
	}
	writeJSON(rw, lean)
}

func (w *world) find(id string) *dto.Project {
	for i := range w.projects {
		if w.projects[i].ID == id || w.projects[i].Name == id {
			return &w.projects[i]
		}
	}
	return nil
}

func (w *world) project(rw http.ResponseWriter, r *http.Request) {
	project := w.find(r.PathValue("id"))
	if project == nil {
		writeError(rw, http.StatusNotFound, errs.New(errs.CodeProjectNotFound, "no such project"))
		return
	}
	writeJSON(rw, project)
}

func (w *world) services(rw http.ResponseWriter, r *http.Request) {
	project := w.find(r.PathValue("id"))
	if project == nil {
		writeError(rw, http.StatusNotFound, errs.New(errs.CodeProjectNotFound, "no such project"))
		return
	}
	writeJSON(rw, project.Services)
}

func (w *world) projectDesire(target dto.ProcessStatus) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		project := w.find(r.PathValue("id"))
		if project == nil {
			writeError(rw, http.StatusNotFound, errs.New(errs.CodeProjectNotFound, "no such project"))
			return
		}
		for i := range project.Services {
			applyDesire(&project.Services[i], target)
		}
		resummarise(project)
		writeJSON(rw, dto.OperationResult{Project: project.ID, Services: project.Services})
	}
}

func (w *world) serviceDesire(target dto.ProcessStatus) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		project := w.find(r.PathValue("id"))
		if project == nil {
			writeError(rw, http.StatusNotFound, errs.New(errs.CodeProjectNotFound, "no such project"))
			return
		}
		name := r.PathValue("name")
		for i := range project.Services {
			if project.Services[i].Name != name {
				continue
			}
			applyDesire(&project.Services[i], target)
			resummarise(project)
			writeJSON(rw, project.Services[i])
			return
		}
		writeError(rw, http.StatusNotFound, errs.New(errs.CodeServiceNotFound, "no such service"))
	}
}

// applyDesire keeps the fixture's states self-consistent: a stopped service has
// no pid, no uptime and no health, and a BLOCKED one stays blocked because its
// precondition is still unmet.
func applyDesire(service *dto.Service, target dto.ProcessStatus) {
	if service.Status == dto.StatusBlocked && target == dto.StatusRunning {
		return
	}
	now := time.Now().UTC()
	if target == dto.StatusRunning {
		service.Status = dto.StatusRunning
		service.DesiredState = dto.DesiredRunning
		service.PID = 4000 + len(service.Name)*7
		service.StartedAt = &now
		service.UptimeSeconds = 1
		service.Observability.LogCapture = dto.LogCaptureAttached
		service.Observability.Adopted = false
		if service.Health.Status == dto.HealthUnknown {
			service.Health.Status = dto.HealthChecking
		}
		return
	}
	service.Status = dto.StatusStopped
	service.DesiredState = dto.DesiredStopped
	service.PID = 0
	service.StartedAt = nil
	service.UptimeSeconds = 0
	service.Health.Status = dto.HealthUnknown
	service.Observability.LogCapture = dto.LogCaptureNone
}

func resummarise(project *dto.Project) {
	summary := dto.ProjectSummary{Total: len(project.Services)}
	for _, service := range project.Services {
		switch service.Status {
		case dto.StatusRunning:
			summary.Running++
			if service.Health.Status == dto.HealthHealthy || service.Health.Status == dto.HealthNotApplicable {
				summary.Healthy++
			}
		case dto.StatusFailed, dto.StatusCrashed, dto.StatusBlocked:
			summary.Failed++
		}
	}
	project.Summary = summary
	switch {
	case summary.Total == 0:
		project.Status = dto.ProjectStopped
	case summary.Failed > 0 && summary.Running == 0:
		project.Status = dto.ProjectFailed
	case summary.Failed > 0 || summary.Healthy < summary.Running:
		project.Status = dto.ProjectDegraded
	case summary.Running == 0:
		project.Status = dto.ProjectStopped
	default:
		project.Status = dto.ProjectHealthy
	}
}

func (w *world) trust(rw http.ResponseWriter, r *http.Request) {
	project := w.find(r.PathValue("id"))
	if project == nil {
		writeError(rw, http.StatusNotFound, errs.New(errs.CodeProjectNotFound, "no such project"))
		return
	}
	var body struct{ Revoke bool }
	_ = json.NewDecoder(r.Body).Decode(&body)
	project.Trusted = !body.Revoke
	writeJSON(rw, project)
}

func (w *world) validate(rw http.ResponseWriter, r *http.Request) {
	writeJSON(rw, sampleValidation())
}

func (w *world) configFile(rw http.ResponseWriter, r *http.Request) {
	project := w.find(r.PathValue("id"))
	if project == nil {
		writeError(rw, http.StatusNotFound, errs.New(errs.CodeProjectNotFound, "no such project"))
		return
	}
	writeJSON(rw, client.ConfigDocument{
		Path:       project.ConfigPath,
		Content:    sampleConfig,
		Validation: sampleValidation(),
		Trusted:    project.Trusted,
	})
}

// sampleValidation carries a warning rather than being simply valid: the config
// page renders findings, and a page that is only ever reviewed in its happy
// state is a page whose warning layout nobody has looked at.
func sampleValidation() *config.ValidationResult {
	return &config.ValidationResult{
		Valid: true,
		Warnings: []*errs.Error{
			errs.New(errs.CodeConfigInvalid,
				"health.interval is shorter than 200ms, which will probe more often than the service can answer").
				At("services.api.health.interval"),
		},
	}
}

func (w *world) inspect(rw http.ResponseWriter, r *http.Request) {
	var body struct{ Path string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Path == "" {
		body.Path = `C:\work\shop`
	}
	writeJSON(rw, registry.Preview{
		Name:        "shop",
		Path:        body.Path,
		ConfigPath:  filepath.Join(body.Path, "devman.yaml"),
		Fingerprint: "sha256:2f6c1d9ab4e77c0e5b8f3a41d2c9e0b7",
		Execution: []config.ExecutionSummary{
			{Service: "web", Runtime: "host", CWD: body.Path, CommandLine: "pnpm dev --port ${PORT}"},
			{Service: "api", Runtime: "host", CWD: filepath.Join(body.Path, "api"),
				CommandLine: "python -m uvicorn app:app --port ${PORT}", EnvFiles: []string{".env.local"}},
			{Service: "redis", Runtime: "docker-compose", CWD: body.Path, Compose: "docker-compose.yml#redis"},
		},
		Validation:        sampleValidation(),
		AlreadyRegistered: false,
		TrustRequired:     true,
	})
}

// register refuses the first time. Registration is the trust boundary, and
// PROJECT_UNTRUSTED is the one refusal every user meets, so it has to be
// reviewable on demand.
func (w *world) register(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		Path  string
		Trust bool
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if !body.Trust {
		writeError(rw, http.StatusForbidden,
			errs.New(errs.CodeProjectUntrusted,
				"this project has not been approved; review what it will run and approve it"))
		return
	}
	now := time.Now().UTC()
	project := dto.Project{
		ID: "prj_new", Name: "shop", Path: body.Path,
		ConfigPath: filepath.Join(body.Path, "devman.yaml"),
		Status:     dto.ProjectStopped, Trusted: true,
		CreatedAt: now, UpdatedAt: now,
	}
	w.projects = append(w.projects, project)
	writeJSON(rw, project)
}

func (w *world) serviceLogs(rw http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	out := make([]dto.LogRecord, 0, len(w.logs))
	for _, record := range w.logs {
		if record.Service == name || name == "" {
			out = append(out, record)
		}
	}
	if len(out) == 0 {
		out = w.logs
	}
	writeJSON(rw, out)
}

func (w *world) logStream(rw http.ResponseWriter, r *http.Request) {
	w.stream(rw, r, func(seq int) (string, any) {
		record := w.logs[seq%len(w.logs)]
		record.Seq = uint64(1000 + seq)
		record.Timestamp = time.Now().UTC()
		return "log", record
	})
}

func (w *world) eventStream(rw http.ResponseWriter, r *http.Request) {
	w.stream(rw, r, func(seq int) (string, any) {
		event := w.events[seq%len(w.events)]
		event.Seq = uint64(1000 + seq)
		event.Timestamp = time.Now().UTC()
		return "event", event
	})
}

// stream keeps the live indicator honest: the topbar shows "live" only while an
// SSE connection is open, so a fixture that did not stream would always be
// reviewed in its reconnecting state.
func (w *world) stream(rw http.ResponseWriter, r *http.Request, next func(seq int) (string, any)) {
	flusher, ok := rw.(http.Flusher)
	if !ok {
		writeError(rw, http.StatusInternalServerError, errs.New(errs.CodeInternal, "streaming unsupported"))
		return
	}
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for seq := 0; ; seq++ {
		name, payload := next(seq)
		body, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(rw, "event: %s\ndata: %s\n\n", name, body)
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *world) portUsage(rw http.ResponseWriter, r *http.Request) {
	port, _ := strconv.Atoi(r.PathValue("port"))
	for i := range w.ports {
		if w.ports[i].Port == port {
			writeJSON(rw, dto.PortUsage{Port: port, Occupied: true, Allocation: &w.ports[i]})
			return
		}
	}
	// An occupied port DevMan does not own is the case the page exists for.
	if port == 5173 {
		writeJSON(rw, dto.PortUsage{Port: port, Occupied: true,
			Owner: &dto.PortOwner{PID: 21188, Name: "node.exe"}})
		return
	}
	writeJSON(rw, dto.PortUsage{Port: port, Occupied: false})
}
