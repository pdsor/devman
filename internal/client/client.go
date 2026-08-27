// Package client talks to the DevMan daemon over the local HTTP API.
//
// Everything above the daemon — the CLI, and later the GUI and the Codex skill —
// goes through this one client, so there is exactly one place where the API
// contract and its error decoding live.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/devman-project/devman/internal/daemon"
	"github.com/devman-project/devman/internal/paths"
	"github.com/devman-project/devman/internal/registry"
	"github.com/devman-project/devman/pkg/config"
	"github.com/devman-project/devman/pkg/dto"
	"github.com/devman-project/devman/pkg/errs"
)

// Client is a connection to a running daemon.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New builds a client for an already resolved endpoint.
func New(endpoint daemon.Endpoint) *Client {
	return &Client{
		base:  endpoint.BaseURL(),
		token: endpoint.Token,
		// No global timeout: the streaming calls are long lived by design and
		// each request-shaped call passes its own context.
		http: &http.Client{},
	}
}

// Connect resolves a running daemon, or fails with DAEMON_NOT_RUNNING.
func Connect(layout paths.Layout) (*Client, error) {
	endpoint, err := daemon.Resolve(layout)
	if err != nil {
		return nil, err
	}
	return New(endpoint), nil
}

// AutoStart connects to the daemon, starting it if necessary.
//
// This is what makes every command work from a cold start: a user should never
// have to know that a daemon exists, let alone start it by hand.
func AutoStart(layout paths.Layout, executable string, timeout time.Duration) (*Client, error) {
	if existing, err := Connect(layout); err == nil {
		return existing, nil
	}
	if executable == "" {
		resolved, err := os.Executable()
		if err != nil {
			return nil, errs.Wrap(errs.CodeInternal, err, "cannot locate the devman executable")
		}
		executable = resolved
	}

	cmd := exec.Command(executable, "daemon", "start", "--foreground")
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "cannot start the DevMan daemon")
	}
	// The daemon is not this process's concern once it is up; releasing it keeps
	// the CLI from becoming its parent for the rest of the session.
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}

	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if started, err := Connect(layout); err == nil {
			return started, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return nil, errs.New(errs.CodeDaemonNotRunning,
		"the DevMan daemon did not become ready within %s", timeout)
}

// --- request plumbing ---

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return errs.Wrap(errs.CodeInternal, err, "cannot encode the request")
		}
		payload = bytes.NewReader(encoded)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	request, err := http.NewRequestWithContext(ctx, method, c.base+path, payload)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot build the request")
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.http.Do(request)
	if err != nil {
		return errs.Wrap(errs.CodeDaemonNotRunning, err, "cannot reach the DevMan daemon")
	}
	defer response.Body.Close()

	if response.StatusCode >= 400 {
		return decodeError(response)
	}
	if out == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot decode the daemon response")
	}
	return nil
}

// decodeError turns an API error body back into the same *errs.Error the daemon
// raised, so a caller can branch on the code rather than on a message.
func decodeError(response *http.Response) error {
	var body struct {
		Error *dto.Error `json:"error"`
	}
	data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err := json.Unmarshal(data, &body); err != nil || body.Error == nil {
		return errs.New(errs.CodeInternal, "daemon returned %s", response.Status)
	}
	converted := errs.New(errs.Code(body.Error.Code), "%s", body.Error.Message)
	if body.Error.Path != "" {
		converted = converted.At(body.Error.Path)
	}
	for key, value := range body.Error.Details {
		converted = converted.With(key, value)
	}
	return converted
}

// --- daemon and environment ---

// DaemonStatus reports what the daemon is doing.
func (c *Client) DaemonStatus() (dto.DaemonStatus, error) {
	var out dto.DaemonStatus
	err := c.do(nil, http.MethodGet, "/daemon/status", nil, &out)
	return out, err
}

// Shutdown stops every service and then the daemon.
func (c *Client) Shutdown() (dto.OperationResult, error) {
	var out dto.OperationResult
	err := c.do(nil, http.MethodPost, "/daemon/shutdown", nil, &out)
	return out, err
}

// Paths reports the daemon's data locations.
func (c *Client) Paths() (dto.Paths, error) {
	var out dto.Paths
	err := c.do(nil, http.MethodGet, "/paths", nil, &out)
	return out, err
}

// Settings returns the flattened global settings.
func (c *Client) Settings() (map[string]string, error) {
	var out map[string]string
	err := c.do(nil, http.MethodGet, "/settings", nil, &out)
	return out, err
}

// Setting returns one settings value.
func (c *Client) Setting(key string) (string, error) {
	var out map[string]string
	if err := c.do(nil, http.MethodGet, "/settings?key="+url.QueryEscape(key), nil, &out); err != nil {
		return "", err
	}
	return out[key], nil
}

// SetSetting writes one settings value.
func (c *Client) SetSetting(key, value string) error {
	body := map[string]string{"key": key, "value": value}
	return c.do(nil, http.MethodPut, "/settings", body, nil)
}

// Tools reports which development tools the daemon can reach.
func (c *Client) Tools() ([]dto.ToolResolution, error) {
	var out []dto.ToolResolution
	err := c.do(nil, http.MethodGet, "/tools", nil, &out)
	return out, err
}

// --- projects ---

// Projects lists registered projects.
func (c *Client) Projects(withServices bool) ([]dto.Project, error) {
	path := "/projects"
	if withServices {
		path += "?services=true"
	}
	var out []dto.Project
	err := c.do(nil, http.MethodGet, path, nil, &out)
	return out, err
}

// Project fetches one project by id.
func (c *Client) Project(id string) (dto.Project, error) {
	var out dto.Project
	err := c.do(nil, http.MethodGet, "/projects/"+url.PathEscape(id), nil, &out)
	return out, err
}

// Resolve turns a selector (id, name or path) plus a working directory into a
// project, which is how a bare `devman start` finds the current project.
func (c *Client) Resolve(selector, cwd string) (dto.Project, error) {
	body := map[string]string{"selector": selector, "cwd": cwd}
	var out dto.Project
	err := c.do(nil, http.MethodPost, "/projects/resolve", body, &out)
	return out, err
}

// Inspect returns what the user must approve before a project may run anything.
func (c *Client) Inspect(path string) (registry.Preview, error) {
	var out registry.Preview
	err := c.do(nil, http.MethodPost, "/projects/inspect", map[string]string{"path": path}, &out)
	return out, err
}

// Register adds a project. Trust must be an explicit decision by the user.
func (c *Client) Register(path string, trust bool) (dto.Project, error) {
	body := map[string]any{"path": path, "trust": trust}
	var out dto.Project
	err := c.do(nil, http.MethodPost, "/projects", body, &out)
	return out, err
}

// Unregister removes a project.
func (c *Client) Unregister(id string) error {
	return c.do(nil, http.MethodDelete, "/projects/"+url.PathEscape(id), nil, nil)
}

// Trust approves (or revokes) the project's current execution fingerprint.
func (c *Client) Trust(id string, revoke bool) (dto.Project, error) {
	var out dto.Project
	err := c.do(nil, http.MethodPost, "/projects/"+url.PathEscape(id)+"/trust",
		map[string]bool{"revoke": revoke}, &out)
	return out, err
}

// Validate checks a project's configuration.
func (c *Client) Validate(id string) (*config.ValidationResult, error) {
	var out config.ValidationResult
	err := c.do(nil, http.MethodGet, "/projects/"+url.PathEscape(id)+"/validate", nil, &out)
	return &out, err
}

// ConfigDocument is a project's devman.yaml as text, with its validation state.
type ConfigDocument struct {
	Path       string                   `json:"path"`
	Content    string                   `json:"content"`
	Validation *config.ValidationResult `json:"validation,omitempty"`
	Trusted    bool                     `json:"trusted"`
}

// ConfigFile reads a project's configuration file.
func (c *Client) ConfigFile(id string) (ConfigDocument, error) {
	var out ConfigDocument
	err := c.do(nil, http.MethodGet, "/projects/"+url.PathEscape(id)+"/config", nil, &out)
	return out, err
}

// SaveConfigFile validates and writes a project's configuration file.
//
// An invalid document is refused rather than written, and the returned error
// carries the validator's own findings so an editor can show them inline.
func (c *Client) SaveConfigFile(id, content string) (ConfigDocument, error) {
	var out ConfigDocument
	err := c.do(nil, http.MethodPut, "/projects/"+url.PathEscape(id)+"/config",
		map[string]string{"content": content}, &out)
	return out, err
}

// Selection is the service set a project-level command applies to.
type Selection struct {
	Services []string `json:"services,omitempty"`
	Profile  string   `json:"profile,omitempty"`
	All      bool     `json:"all,omitempty"`
}

// StartProject starts a set of services in dependency order.
func (c *Client) StartProject(id string, selection Selection) (dto.OperationResult, error) {
	return c.projectAction(id, "start", selection)
}

// StopProject stops a set of services in reverse dependency order.
func (c *Client) StopProject(id string, selection Selection) (dto.OperationResult, error) {
	return c.projectAction(id, "stop", selection)
}

// RestartProject stops and starts, re-reading devman.yaml in between.
func (c *Client) RestartProject(id string, selection Selection) (dto.OperationResult, error) {
	return c.projectAction(id, "restart", selection)
}

func (c *Client) projectAction(id, action string, selection Selection) (dto.OperationResult, error) {
	var out dto.OperationResult
	err := c.do(nil, http.MethodPost,
		"/projects/"+url.PathEscape(id)+"/"+action, selection, &out)
	return out, err
}

// --- services ---

// Services lists a project's services with their state.
func (c *Client) Services(projectID string) ([]dto.Service, error) {
	var out []dto.Service
	err := c.do(nil, http.MethodGet, "/projects/"+url.PathEscape(projectID)+"/services", nil, &out)
	return out, err
}

// Service fetches one service.
func (c *Client) Service(projectID, name string) (dto.Service, error) {
	var out dto.Service
	err := c.do(nil, http.MethodGet, c.servicePath(projectID, name), nil, &out)
	return out, err
}

// StartService starts one service.
func (c *Client) StartService(projectID, name string) (dto.Service, error) {
	return c.serviceAction(projectID, name, "start")
}

// StopService stops one service and records that it must stay stopped.
func (c *Client) StopService(projectID, name string) (dto.Service, error) {
	return c.serviceAction(projectID, name, "stop")
}

// RestartService restarts one service.
func (c *Client) RestartService(projectID, name string) (dto.Service, error) {
	return c.serviceAction(projectID, name, "restart")
}

func (c *Client) serviceAction(projectID, name, action string) (dto.Service, error) {
	var out dto.Service
	err := c.do(nil, http.MethodPost, c.servicePath(projectID, name)+"/"+action, nil, &out)
	return out, err
}

func (c *Client) servicePath(projectID, name string) string {
	return "/projects/" + url.PathEscape(projectID) + "/services/" + url.PathEscape(name)
}

// LogQuery filters a log request.
type LogQuery struct {
	Tail   int
	Stream string
	Since  time.Time
}

func (q LogQuery) encode() string {
	values := url.Values{}
	if q.Tail > 0 {
		values.Set("tail", strconv.Itoa(q.Tail))
	}
	if q.Stream != "" {
		values.Set("stream", q.Stream)
	}
	if !q.Since.IsZero() {
		values.Set("since", q.Since.Format(time.RFC3339Nano))
	}
	if len(values) == 0 {
		return ""
	}
	return "?" + values.Encode()
}

// Logs returns captured history for one service.
func (c *Client) Logs(projectID, name string, query LogQuery) ([]dto.LogRecord, error) {
	var out []dto.LogRecord
	err := c.do(nil, http.MethodGet, c.servicePath(projectID, name)+"/logs"+query.encode(), nil, &out)
	return out, err
}

// StreamLogs follows a service's output until the context is cancelled or the
// callback returns an error.
func (c *Client) StreamLogs(
	ctx context.Context, projectID, name string, tail int, onRecord func(dto.LogRecord) error,
) error {
	path := c.servicePath(projectID, name) + "/logs/stream?tail=" + strconv.Itoa(tail)
	return c.stream(ctx, path, func(payload []byte) error {
		var record dto.LogRecord
		if err := json.Unmarshal(payload, &record); err != nil {
			return nil
		}
		return onRecord(record)
	})
}

// --- ports and events ---

// Ports lists every active allocation.
func (c *Client) Ports() ([]dto.PortAllocation, error) {
	var out []dto.PortAllocation
	err := c.do(nil, http.MethodGet, "/ports", nil, &out)
	return out, err
}

// PortUsage answers who holds a port, including foreign processes.
func (c *Client) PortUsage(port int) (dto.PortUsage, error) {
	var out dto.PortUsage
	err := c.do(nil, http.MethodGet, "/ports/"+strconv.Itoa(port), nil, &out)
	return out, err
}

// Events returns recent daemon events, oldest first.
func (c *Client) Events(limit int) ([]dto.Event, error) {
	var out []dto.Event
	err := c.do(nil, http.MethodGet, "/events?limit="+strconv.Itoa(limit), nil, &out)
	return out, err
}

// StreamEvents follows the daemon event bus.
func (c *Client) StreamEvents(ctx context.Context, replay int, onEvent func(dto.Event) error) error {
	return c.stream(ctx, "/events/stream?replay="+strconv.Itoa(replay), func(payload []byte) error {
		var event dto.Event
		if err := json.Unmarshal(payload, &event); err != nil {
			return nil
		}
		return onEvent(event)
	})
}

// stream consumes a server-sent event response.
//
// Only the `data:` lines carry payloads; keep-alive comments are ignored, which
// is how a dropped connection is noticed without a heartbeat protocol.
func (c *Client) stream(ctx context.Context, path string, onData func([]byte) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot build the stream request")
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "text/event-stream")

	response, err := c.http.Do(request)
	if err != nil {
		return errs.Wrap(errs.CodeDaemonNotRunning, err, "cannot reach the DevMan daemon")
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return decodeError(response)
	}

	reader := bufio.NewReaderSize(response.Body, 64*1024)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return nil
			}
			return errs.Wrap(errs.CodeInternal, err, "the stream ended unexpectedly")
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if err := onData([]byte(payload)); err != nil {
			return err
		}
	}
}

// Endpoint reports where this client is pointed, for diagnostics.
func (c *Client) Endpoint() string { return c.base }

// String makes a client printable without leaking its token.
func (c *Client) String() string { return fmt.Sprintf("devman client(%s)", c.base) }
