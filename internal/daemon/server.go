package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/devman-project/devman/internal/events"
	"github.com/devman-project/devman/internal/logstore"
	"github.com/devman-project/devman/internal/paths"
	"github.com/devman-project/devman/internal/portmgr"
	"github.com/devman-project/devman/internal/registry"
	"github.com/devman-project/devman/internal/settings"
	"github.com/devman-project/devman/internal/storage"
	"github.com/devman-project/devman/internal/supervisor"
	"github.com/devman-project/devman/pkg/dto"
	"github.com/devman-project/devman/pkg/errs"
)

// Options are the collaborators the HTTP layer serves.
type Options struct {
	Layout     paths.Layout
	Settings   *settings.Settings
	DB         *storage.DB
	Registry   *registry.Registry
	Ports      *portmgr.Manager
	Logs       *logstore.Manager
	Events     *events.Bus
	Supervisor *supervisor.Supervisor
	Version    string
}

// Server is the daemon's HTTP API.
type Server struct {
	opts     Options
	listener *Listener
	http     *http.Server

	shutdownOnce sync.Once
	shutdownReq  chan struct{}
}

// NewServer wires the routes onto a bound listener.
func NewServer(listener *Listener, opts Options) *Server {
	s := &Server{
		opts:        opts,
		listener:    listener,
		shutdownReq: make(chan struct{}),
	}
	s.http = &http.Server{
		Handler: s.handler(),
		// The API is local and its handlers are short; a read timeout protects
		// against a stuck client without affecting SSE, which writes only.
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Serve blocks until the server is shut down.
func (s *Server) Serve() error {
	err := s.http.Serve(s.listener.Net)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting requests and waits for in-flight ones.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// ShutdownRequested is closed when a client calls the shutdown endpoint.
func (s *Server) ShutdownRequested() <-chan struct{} { return s.shutdownReq }

func (s *Server) requestShutdown() {
	s.shutdownOnce.Do(func() { close(s.shutdownReq) })
}

// handler builds the route table. Patterns use the method and path wildcards of
// net/http, so routing needs no third-party router.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	base := "/api/" + APIVersion

	mux.HandleFunc("GET "+base+"/daemon/status", s.handleDaemonStatus)
	mux.HandleFunc("POST "+base+"/daemon/shutdown", s.handleDaemonShutdown)
	mux.HandleFunc("GET "+base+"/paths", s.handlePaths)
	mux.HandleFunc("GET "+base+"/settings", s.handleSettingsGet)
	mux.HandleFunc("PUT "+base+"/settings", s.handleSettingsSet)
	mux.HandleFunc("GET "+base+"/tools", s.handleTools)

	mux.HandleFunc("GET "+base+"/projects", s.handleProjectList)
	mux.HandleFunc("POST "+base+"/projects", s.handleProjectRegister)
	mux.HandleFunc("POST "+base+"/projects/inspect", s.handleProjectInspect)
	mux.HandleFunc("POST "+base+"/projects/resolve", s.handleProjectResolve)
	mux.HandleFunc("GET "+base+"/projects/{id}", s.handleProjectGet)
	mux.HandleFunc("DELETE "+base+"/projects/{id}", s.handleProjectUnregister)
	mux.HandleFunc("POST "+base+"/projects/{id}/trust", s.handleProjectTrust)
	mux.HandleFunc("GET "+base+"/projects/{id}/validate", s.handleProjectValidate)
	mux.HandleFunc("GET "+base+"/projects/{id}/config", s.handleProjectConfigGet)
	mux.HandleFunc("PUT "+base+"/projects/{id}/config", s.handleProjectConfigSet)
	mux.HandleFunc("POST "+base+"/projects/{id}/start", s.handleProjectStart)
	mux.HandleFunc("POST "+base+"/projects/{id}/stop", s.handleProjectStop)
	mux.HandleFunc("POST "+base+"/projects/{id}/restart", s.handleProjectRestart)

	mux.HandleFunc("GET "+base+"/projects/{id}/services", s.handleServiceList)
	mux.HandleFunc("GET "+base+"/projects/{id}/services/{name}", s.handleServiceGet)
	mux.HandleFunc("POST "+base+"/projects/{id}/services/{name}/start", s.handleServiceStart)
	mux.HandleFunc("POST "+base+"/projects/{id}/services/{name}/stop", s.handleServiceStop)
	mux.HandleFunc("POST "+base+"/projects/{id}/services/{name}/restart", s.handleServiceRestart)
	mux.HandleFunc("GET "+base+"/projects/{id}/services/{name}/logs", s.handleServiceLogs)
	mux.HandleFunc("GET "+base+"/projects/{id}/services/{name}/logs/stream", s.handleServiceLogStream)

	mux.HandleFunc("GET "+base+"/ports", s.handlePortList)
	mux.HandleFunc("GET "+base+"/ports/{port}", s.handlePortUsage)

	mux.HandleFunc("GET "+base+"/events", s.handleEventList)
	mux.HandleFunc("GET "+base+"/events/stream", s.handleEventStream)

	mux.HandleFunc("/", s.handleNotFound)

	return s.recoverPanics(s.checkOrigin(s.requireToken(mux)))
}

// recoverPanics keeps one broken handler from taking the daemon down and losing
// every supervised service's log capture with it.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				writeError(w, errs.New(errs.CodeInternal, "internal error: %v", recovered))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// requireToken authenticates every request with the local bearer token.
//
// The two streaming endpoints also accept `?token=`, because the browser
// EventSource API cannot set headers. Nothing else does, so a token cannot leak
// into a server log for an ordinary call.
func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		presented := bearerToken(r)
		if presented == "" && strings.HasSuffix(r.URL.Path, "/stream") {
			presented = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(presented), []byte(s.listener.Token)) != 1 {
			writeError(w, errs.New(errs.CodeUnauthorized,
				"a valid DevMan auth token is required"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if header == "" {
		return ""
	}
	scheme, value, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(value)
}

// checkOrigin blocks cross-site requests from a browser.
//
// There is no permissive CORS here: a page on the internet must not be able to
// drive a local daemon that can start processes. Only loopback origins and the
// Tauri shell are echoed back.
func (s *Server) checkOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if !originAllowed(origin) {
				writeError(w, errs.New(errs.CodeUnauthorized,
					"origin %s is not allowed to talk to the DevMan daemon", origin))
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originAllowed accepts loopback pages and the Tauri shell only.
func originAllowed(origin string) bool {
	if origin == "null" {
		// A file:// page. It cannot read a cross-origin response anyway, and
		// treating it as allowed would weaken the check for no benefit.
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch parsed.Scheme {
	case "tauri":
		return true
	case "http", "https":
	default:
		return false
	}
	host := parsed.Hostname()
	switch host {
	case "127.0.0.1", "::1", "localhost", "tauri.localhost":
		return true
	default:
		return false
	}
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, errs.New(errs.CodeInvalidRequest, "no such endpoint: %s %s", r.Method, r.URL.Path))
}

// writeJSON renders any DTO.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(body)
}

// writeError maps a DevMan error onto its HTTP status, so the CLI and the GUI
// can act on the code rather than on the status alone.
func writeError(w http.ResponseWriter, err error) {
	converted := errs.From(err)
	writeJSON(w, converted.HTTPStatus(), map[string]any{"error": dto.FromError(converted)})
}

// decodeBody reads a JSON request body, tolerating an empty one.
func decodeBody(r *http.Request, target any) error {
	if r.Body == nil {
		return nil
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return errs.New(errs.CodeInvalidRequest, "invalid request body: %v", err)
	}
	return nil
}

// intParam reads a positive integer query parameter.
func intParam(r *http.Request, name string, fallback int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

// sseWriter is the shared shape of the two streaming endpoints.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newSSEWriter(w http.ResponseWriter) (*sseWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errs.New(errs.CodeInternal, "streaming is not supported by this server")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Disables buffering in any proxy a user might put in front of the GUI.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &sseWriter{w: w, flusher: flusher}, nil
}

func (s *sseWriter) send(event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

func (s *sseWriter) comment(text string) error {
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// logDTO converts a captured record to its wire form.
func logDTO(record logstore.Record) dto.LogRecord {
	return dto.LogRecord{
		Seq:       record.Seq,
		Timestamp: record.Timestamp,
		Project:   record.Project,
		Service:   record.Service,
		Stream:    string(record.Stream),
		Message:   record.Message,
	}
}

// projectDTO renders a project with its services and aggregate status.
func (s *Server) projectDTO(record storage.ProjectRecord, withServices bool) dto.Project {
	out := dto.Project{
		ID:         record.ID,
		Name:       record.Name,
		Path:       record.Path,
		ConfigPath: record.ConfigPath,
		Favorite:   record.Favorite,
		CreatedAt:  record.CreatedAt,
		UpdatedAt:  record.UpdatedAt,
	}
	if record.LastStartedAt != nil {
		started := *record.LastStartedAt
		out.LastStartedAt = &started
	}

	cfg, err := s.opts.Registry.Config(record.ID)
	if err != nil {
		out.ConfigError = dto.FromError(err)
		out.Status = dto.ProjectFailed
		return out
	}
	out.DisplayName = registry.ProjectDisplayName(record, cfg)
	out.Trusted = record.TrustedFingerprint != "" &&
		record.TrustedFingerprint == cfg.ExecutionFingerprint()

	services := s.opts.Supervisor.ProjectServices(record.ID, cfg)
	summary, status := supervisor.Summarise(services)
	out.Summary = summary
	out.Status = status
	if withServices {
		out.Services = services
	}
	return out
}
