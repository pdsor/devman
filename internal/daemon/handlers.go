package daemon

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/devman-project/devman/internal/envresolve"
	"github.com/devman-project/devman/internal/logstore"
	"github.com/devman-project/devman/pkg/dto"
	"github.com/devman-project/devman/pkg/errs"
)

// --- daemon and environment ---

func (s *Server) handleDaemonStatus(w http.ResponseWriter, _ *http.Request) {
	projects, err := s.opts.Registry.Projects()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dto.DaemonStatus{
		Info:     s.listener.Info,
		Uptime:   int64(time.Since(s.listener.Info.StartedAt).Seconds()),
		Projects: len(projects),
		Running:  s.opts.Supervisor.RunningCount(),
		DataDir:  s.opts.Layout.Home,
		LogsDir:  s.opts.Layout.Logs,
		Healthy:  true,
	})
}

// handleDaemonShutdown stops every service and then the daemon.
//
// Stopping the services is the point: the daemon exiting must not leave
// processes behind that nothing is watching.
func (s *Server) handleDaemonShutdown(w http.ResponseWriter, _ *http.Request) {
	stopped := s.opts.Supervisor.StopAll()
	writeJSON(w, http.StatusOK, dto.OperationResult{Services: stopped})
	s.requestShutdown()
}

func (s *Server) handlePaths(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, dto.Paths{
		Home:      s.opts.Layout.Home,
		Settings:  s.opts.Layout.Settings,
		Database:  s.opts.Layout.Database,
		Daemon:    s.opts.Layout.Daemon,
		AuthToken: s.opts.Layout.AuthToken,
		Logs:      s.opts.Layout.Logs,
	})
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	if key := r.URL.Query().Get("key"); key != "" {
		value, err := s.opts.Settings.Get(key)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{key: value})
		return
	}
	writeJSON(w, http.StatusOK, s.opts.Settings.Flatten())
}

// handleSettingsSet applies one key at a time, revalidating the whole file.
// An invalid edit is refused rather than persisted, so the daemon can never be
// left with settings it would fail to start from.
func (s *Server) handleSettingsSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, err)
		return
	}
	if body.Key == "" {
		writeError(w, errs.New(errs.CodeInvalidRequest, "a settings key is required"))
		return
	}
	if err := s.opts.Settings.Set(body.Key, body.Value); err != nil {
		writeError(w, err)
		return
	}
	if err := s.opts.Settings.Save(s.opts.Layout.Settings); err != nil {
		writeError(w, err)
		return
	}
	// The port manager reads ranges from the settings it was given.
	s.opts.Ports.SetSettings(s.opts.Settings)
	writeJSON(w, http.StatusOK, map[string]string{body.Key: body.Value})
}

// handleTools reports which development tools the daemon can actually reach,
// which is how a GUI launched with a reduced PATH is diagnosed.
func (s *Server) handleTools(w http.ResponseWriter, _ *http.Request) {
	resolver := s.toolResolver()
	found := resolver.ProbeTools(currentEnv())

	out := make([]dto.ToolResolution, 0, len(found))
	for _, name := range toolNames() {
		path, ok := found[name]
		out = append(out, dto.ToolResolution{Name: name, Path: path, Found: ok})
	}
	writeJSON(w, http.StatusOK, out)
}

// --- projects ---

func (s *Server) handleProjectList(w http.ResponseWriter, r *http.Request) {
	records, err := s.opts.Registry.Projects()
	if err != nil {
		writeError(w, err)
		return
	}
	withServices := r.URL.Query().Get("services") == "true"
	out := make([]dto.Project, 0, len(records))
	for _, record := range records {
		out = append(out, s.projectDTO(record, withServices))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleProjectRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path  string `json:"path"`
		Trust bool   `json:"trust"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, err)
		return
	}
	if body.Path == "" {
		writeError(w, errs.New(errs.CodeInvalidRequest, "a project path is required"))
		return
	}
	record, err := s.opts.Registry.Register(body.Path, body.Trust)
	if err != nil {
		writeError(w, err)
		return
	}
	s.opts.Events.Emit(dto.EventProjectRegistered, record.ID, "",
		"registered "+record.Name, map[string]any{"path": record.Path})
	writeJSON(w, http.StatusCreated, s.projectDTO(record, true))
}

// handleProjectInspect returns what the user must approve before a project may
// execute anything. The trust decision itself is never made here.
func (s *Server) handleProjectInspect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, err)
		return
	}
	preview, err := s.opts.Registry.Inspect(body.Path)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (s *Server) handleProjectGet(w http.ResponseWriter, r *http.Request) {
	record, err := s.opts.Registry.Project(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.projectDTO(record, true))
}

func (s *Server) handleProjectUnregister(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.projectHasRunningServices(id) {
		writeError(w, errs.New(errs.CodeAlreadyRunning,
			"stop the project's services before unregistering it"))
		return
	}
	if err := s.opts.Registry.Unregister(id); err != nil {
		writeError(w, err)
		return
	}
	s.opts.Events.Emit(dto.EventProjectUnregistered, id, "", "unregistered", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) projectHasRunningServices(id string) bool {
	cfg, err := s.opts.Registry.Config(id)
	if err != nil {
		return false
	}
	for _, svc := range s.opts.Supervisor.ProjectServices(id, cfg) {
		if svc.Status == dto.StatusRunning || svc.Status == dto.StatusStarting {
			return true
		}
	}
	return false
}

// handleProjectTrust approves the project's current execution fingerprint.
func (s *Server) handleProjectTrust(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Revoke bool `json:"revoke"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, err)
		return
	}
	var err error
	if body.Revoke {
		err = s.opts.Registry.Revoke(id)
	} else {
		err = s.opts.Registry.Trust(id)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	record, err := s.opts.Registry.Project(id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.projectDTO(record, false))
}

func (s *Server) handleProjectValidate(w http.ResponseWriter, r *http.Request) {
	_, result, err := s.opts.Registry.ConfigWithValidation(r.PathValue("id"))
	if err != nil && result == nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// serviceSelection is the body shared by the project-level lifecycle calls.
type serviceSelection struct {
	Services []string `json:"services,omitempty"`
	Profile  string   `json:"profile,omitempty"`
	All      bool     `json:"all,omitempty"`
}

func (s *Server) handleProjectStart(w http.ResponseWriter, r *http.Request) {
	var body serviceSelection
	if err := decodeBody(r, &body); err != nil {
		writeError(w, err)
		return
	}
	result, err := s.opts.Supervisor.StartProject(r.PathValue("id"), body.Services, body.Profile, body.All)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleProjectStop(w http.ResponseWriter, r *http.Request) {
	var body serviceSelection
	if err := decodeBody(r, &body); err != nil {
		writeError(w, err)
		return
	}
	result, err := s.opts.Supervisor.StopProject(r.PathValue("id"), body.Services, body.All)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleProjectRestart stops and then starts, so the configuration is re-read in
// between and an edit takes effect.
func (s *Server) handleProjectRestart(w http.ResponseWriter, r *http.Request) {
	var body serviceSelection
	if err := decodeBody(r, &body); err != nil {
		writeError(w, err)
		return
	}
	id := r.PathValue("id")
	if _, err := s.opts.Supervisor.StopProject(id, body.Services, body.All); err != nil {
		writeError(w, err)
		return
	}
	result, err := s.opts.Supervisor.StartProject(id, body.Services, body.Profile, body.All)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// --- services ---

func (s *Server) handleServiceList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cfg, err := s.opts.Registry.Config(id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.opts.Supervisor.ProjectServices(id, cfg))
}

func (s *Server) handleServiceGet(w http.ResponseWriter, r *http.Request) {
	status, err := s.opts.Supervisor.ServiceStatus(r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleServiceStart(w http.ResponseWriter, r *http.Request) {
	status, err := s.opts.Supervisor.StartService(r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleServiceStop(w http.ResponseWriter, r *http.Request) {
	status, err := s.opts.Supervisor.StopService(r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleServiceRestart(w http.ResponseWriter, r *http.Request) {
	status, err := s.opts.Supervisor.RestartService(r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleServiceLogs returns captured history. `since` accepts RFC3339 so an
// agent can ask for "what happened after the last thing I saw".
func (s *Server) handleServiceLogs(w http.ResponseWriter, r *http.Request) {
	serviceLog, err := s.serviceLog(r)
	if err != nil {
		writeError(w, err)
		return
	}
	query := logstore.Query{
		Tail:   intParam(r, "tail", 200),
		Stream: logstore.Stream(r.URL.Query().Get("stream")),
	}
	if raw := r.URL.Query().Get("since"); raw != "" {
		since, parseErr := time.Parse(time.RFC3339Nano, raw)
		if parseErr != nil {
			writeError(w, errs.New(errs.CodeInvalidRequest,
				"since must be an RFC3339 timestamp, got %q", raw))
			return
		}
		query.Since = since
	}

	records := serviceLog.History(query)
	out := make([]dto.LogRecord, 0, len(records))
	for _, record := range records {
		out = append(out, logDTO(record))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleServiceLogStream is the log SSE endpoint. It is deliberately separate
// from the event stream: a GUI showing one service's output must not have to
// filter the whole daemon's event traffic.
func (s *Server) handleServiceLogStream(w http.ResponseWriter, r *http.Request) {
	serviceLog, err := s.serviceLog(r)
	if err != nil {
		writeError(w, err)
		return
	}
	stream, err := newSSEWriter(w)
	if err != nil {
		writeError(w, err)
		return
	}

	// Replay a little history first so a fresh subscriber has context.
	if tail := intParam(r, "tail", 50); tail > 0 {
		for _, record := range serviceLog.History(logstore.Query{Tail: tail}) {
			if sendErr := stream.send("log", logDTO(record)); sendErr != nil {
				return
			}
		}
	}

	records, cancel := serviceLog.Subscribe(256)
	defer cancel()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case record, open := <-records:
			if !open {
				return
			}
			if sendErr := stream.send("log", logDTO(record)); sendErr != nil {
				return
			}
		case <-ticker.C:
			// A keep-alive comment detects a dead client and keeps proxies from
			// closing an idle stream.
			if err := stream.comment("keep-alive"); err != nil {
				return
			}
		}
	}
}

func (s *Server) serviceLog(r *http.Request) (*logstore.ServiceLog, error) {
	projectID, name := r.PathValue("id"), r.PathValue("name")
	cfg, err := s.opts.Registry.Config(projectID)
	if err != nil {
		return nil, err
	}
	if _, err := cfg.Service(name); err != nil {
		return nil, err
	}
	return s.opts.Logs.Service(projectID, name)
}

// --- ports ---

func (s *Server) handlePortList(w http.ResponseWriter, _ *http.Request) {
	allocations, err := s.opts.Ports.List()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, allocations)
}

// handlePortUsage answers "who has this port", including processes DevMan does
// not manage. An unresolvable owner is reported as null rather than as an error.
func (s *Server) handlePortUsage(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		writeError(w, errs.New(errs.CodeInvalidRequest, "%q is not a port number", r.PathValue("port")))
		return
	}
	usage, err := s.opts.Ports.Usage(port)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

// --- events ---

func (s *Server) handleEventList(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 100)
	records, err := s.opts.DB.Events(limit)
	if err != nil {
		writeError(w, err)
		return
	}
	// The database returns newest first; the API returns chronological order,
	// which is what a consumer appending to a list wants.
	out := make([]dto.Event, 0, len(records))
	for i := len(records) - 1; i >= 0; i-- {
		record := records[i]
		out = append(out, dto.Event{
			Seq:       record.Seq,
			Type:      dto.EventType(record.Type),
			Timestamp: record.CreatedAt,
			Project:   record.ProjectID,
			Service:   record.ServiceName,
			Message:   record.Message,
			Data:      record.Data,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleEventStream is the daemon-wide event SSE endpoint.
func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	stream, err := newSSEWriter(w)
	if err != nil {
		writeError(w, err)
		return
	}

	if replay := intParam(r, "replay", 20); replay > 0 {
		for _, event := range s.opts.Events.Recent(replay) {
			if sendErr := stream.send("event", event); sendErr != nil {
				return
			}
		}
	}

	updates, cancel := s.opts.Events.Subscribe(256)
	defer cancel()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-updates:
			if !open {
				return
			}
			if sendErr := stream.send("event", event); sendErr != nil {
				return
			}
		case <-ticker.C:
			if err := stream.comment("keep-alive"); err != nil {
				return
			}
		}
	}
}

// --- helpers that keep the handlers readable ---

// shutdownGrace bounds how long in-flight requests may take once a shutdown has
// been requested.
const shutdownGrace = 10 * time.Second

// GracefulShutdown stops the HTTP server and releases the discovery record.
func (s *Server) GracefulShutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	shutdownErr := s.Shutdown(ctx)
	releaseErr := s.listener.Release()
	if shutdownErr != nil {
		return shutdownErr
	}
	return releaseErr
}

// handleProjectResolve turns a user-supplied selector into a project.
//
// Resolution lives in the daemon because it needs the registry: a bare
// `devman start` has to find the project containing the caller's working
// directory, and the CLI must not have to open the database to do that.
func (s *Server) handleProjectResolve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Selector string `json:"selector,omitempty"`
		CWD      string `json:"cwd,omitempty"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, err)
		return
	}
	record, err := s.opts.Registry.Resolve(body.Selector, body.CWD)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.projectDTO(record, true))
}

// toolResolver builds the executable resolver from the global settings.
func (s *Server) toolResolver() envresolve.Resolver {
	return envresolve.Resolver{
		AdditionalPath: s.opts.Settings.Environment.AdditionalPath,
		ExtraEnv:       s.opts.Settings.Environment.Env,
	}
}

func currentEnv() map[string]string { return envresolve.CurrentEnv() }

func toolNames() []string { return envresolve.ToolNames }
