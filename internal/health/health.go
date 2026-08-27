// Package health probes services and tracks health separately from process
// state.
//
// A service can be RUNNING and UNHEALTHY at the same time; conflating the two
// is what makes "it's up" useless. Health is therefore never inferred from the
// process, and DevMan never guesses a tcp or http probe from the presence of
// ports: an undeclared health check means process health, nothing more.
package health

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/devman-project/devman/pkg/config"
	"github.com/devman-project/devman/pkg/dto"
)

// Spec is a fully resolved health check: templates expanded, ports substituted.
type Spec struct {
	Type           config.HealthKind
	URL            string
	Host           string
	Port           int
	ExpectedStatus []int
	Interval       time.Duration
	Timeout        time.Duration
	Retries        int
}

// Target describes what is probed, for display.
func (s Spec) Target() string {
	switch s.Type {
	case config.HealthHTTP:
		return s.URL
	case config.HealthTCP:
		host := s.Host
		if host == "" {
			host = "127.0.0.1"
		}
		return net.JoinHostPort(host, strconv.Itoa(s.Port))
	default:
		return "process"
	}
}

// Result is one probe outcome.
type Result struct {
	Status    dto.HealthStatus
	Target    string
	CheckedAt time.Time
	Latency   time.Duration
	Message   string
	Failures  int
}

// DTO converts a result for the API.
func (r Result) DTO(kind config.HealthKind) dto.HealthResult {
	out := dto.HealthResult{
		Status:    r.Status,
		Type:      string(kind),
		Target:    r.Target,
		LatencyMS: r.Latency.Milliseconds(),
		Failures:  r.Failures,
		Message:   r.Message,
	}
	if !r.CheckedAt.IsZero() {
		checked := r.CheckedAt
		out.CheckedAt = &checked
	}
	return out
}

// AliveFunc reports whether the supervised process is still running.
type AliveFunc func() bool

// httpClient is shared so keep-alives do not accumulate one client per service.
// Redirects are not followed: a health endpoint that redirects is answering
// about something else.
var httpClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
	},
}

// Check runs a single probe.
func Check(ctx context.Context, spec Spec, alive AliveFunc) Result {
	started := time.Now()
	result := Result{Target: spec.Target(), CheckedAt: started.UTC()}

	// Nothing can be healthy if the process is gone. For process-type checks
	// this is the whole test; for tcp and http it avoids reporting a stale
	// success from another listener that happens to hold the port.
	if alive != nil && !alive() {
		result.Status = dto.HealthUnhealthy
		result.Message = "process is not running"
		result.Latency = time.Since(started)
		return result
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}

	switch spec.Type {
	case config.HealthProcess:
		result.Status = dto.HealthNotApplicable
		result.Message = "process health only"
	case config.HealthTCP:
		host := spec.Host
		if host == "" {
			host = "127.0.0.1"
		}
		address := net.JoinHostPort(host, strconv.Itoa(spec.Port))
		dialer := net.Dialer{Timeout: timeout}
		conn, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			result.Status = dto.HealthUnhealthy
			result.Message = err.Error()
		} else {
			conn.Close()
			result.Status = dto.HealthHealthy
		}
	case config.HealthHTTP:
		requestCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, spec.URL, nil)
		if err != nil {
			result.Status = dto.HealthUnhealthy
			result.Message = err.Error()
			break
		}
		response, err := httpClient.Do(request)
		if err != nil {
			result.Status = dto.HealthUnhealthy
			result.Message = err.Error()
			break
		}
		response.Body.Close()
		if statusAccepted(response.StatusCode, spec.ExpectedStatus) {
			result.Status = dto.HealthHealthy
		} else {
			result.Status = dto.HealthUnhealthy
			result.Message = fmt.Sprintf("unexpected status %d", response.StatusCode)
		}
	default:
		result.Status = dto.HealthUnknown
		result.Message = "unknown health type " + string(spec.Type)
	}

	result.Latency = time.Since(started)
	return result
}

// statusAccepted implements the default of "any 2xx" when expected_status is
// not declared.
func statusAccepted(status int, expected []int) bool {
	if len(expected) == 0 {
		return status >= 200 && status < 300
	}
	for _, want := range expected {
		if status == want {
			return true
		}
	}
	return false
}

// Monitor probes one service on an interval and reports transitions.
type Monitor struct {
	spec     Spec
	alive    AliveFunc
	onChange func(Result)

	mu       sync.Mutex
	current  Result
	failures int

	cancel  context.CancelFunc
	stopped chan struct{}
	healthy chan struct{}
	once    sync.Once
}

// NewMonitor creates a monitor. onChange is called only when the reported
// status differs from the previous one, so subscribers see transitions rather
// than a stream of identical results.
func NewMonitor(spec Spec, alive AliveFunc, onChange func(Result)) *Monitor {
	return &Monitor{
		spec:     spec,
		alive:    alive,
		onChange: onChange,
		current:  Result{Status: dto.HealthUnknown, Target: spec.Target()},
		stopped:  make(chan struct{}),
		healthy:  make(chan struct{}),
	}
}

// Start begins probing in the background.
func (m *Monitor) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	m.cancel = cancel

	interval := m.spec.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}

	// Process-only health has nothing to poll: it is answered by the supervisor
	// observing the process, so the monitor reports N/A once and stops.
	if m.spec.Type == config.HealthProcess {
		m.set(Result{
			Status:    dto.HealthNotApplicable,
			Target:    m.spec.Target(),
			CheckedAt: time.Now().UTC(),
			Message:   "process health only",
		})
		m.signalHealthy()
		close(m.stopped)
		cancel()
		return
	}

	m.set(Result{Status: dto.HealthChecking, Target: m.spec.Target()})

	go func() {
		defer close(m.stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			result := Check(ctx, m.spec, m.alive)
			m.record(result)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (m *Monitor) record(result Result) {
	m.mu.Lock()
	if result.Status == dto.HealthHealthy {
		m.failures = 0
	} else if result.Status == dto.HealthUnhealthy {
		m.failures++
	}
	result.Failures = m.failures
	previous := m.current.Status
	m.current = result
	m.mu.Unlock()

	if result.Status == dto.HealthHealthy {
		m.signalHealthy()
	}
	if previous != result.Status && m.onChange != nil {
		m.onChange(result)
	}
}

func (m *Monitor) set(result Result) {
	m.mu.Lock()
	m.current = result
	m.mu.Unlock()
}

func (m *Monitor) signalHealthy() {
	m.once.Do(func() { close(m.healthy) })
}

// Current returns the latest result.
func (m *Monitor) Current() Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// Stop ends probing.
func (m *Monitor) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	<-m.stopped
}

// WaitHealthy blocks until the service reports healthy, the context ends or the
// timeout elapses. Process-only health is considered satisfied immediately,
// which is what makes `condition: started` and `condition: healthy` behave
// sensibly for services without a real probe.
func (m *Monitor) WaitHealthy(ctx context.Context, timeout time.Duration) (Result, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-m.healthy:
		return m.Current(), nil
	case <-ctx.Done():
		return m.Current(), ctx.Err()
	case <-timer.C:
		return m.Current(), context.DeadlineExceeded
	}
}

// SpecFrom resolves a declared health check into a probe spec, expanding
// templates against the service's ports and environment.
func SpecFrom(
	declared *config.HealthSpec,
	expand func(string) (string, error),
	defaults Defaults,
) (Spec, error) {
	spec := Spec{
		Type:     config.HealthProcess,
		Interval: defaults.Interval,
		Timeout:  defaults.Timeout,
		Retries:  defaults.Retries,
	}
	if declared == nil {
		return spec, nil
	}
	spec.Type = declared.Type
	spec.ExpectedStatus = declared.ExpectedStatus
	if declared.Interval != nil {
		spec.Interval = declared.Interval.Duration
	}
	if declared.Timeout != nil {
		spec.Timeout = declared.Timeout.Duration
	}
	if declared.Retries > 0 {
		spec.Retries = declared.Retries
	}

	switch declared.Type {
	case config.HealthHTTP:
		url, err := expand(declared.URL)
		if err != nil {
			return Spec{}, err
		}
		spec.URL = url
	case config.HealthTCP:
		host := declared.Host
		if host == "" {
			host = "127.0.0.1"
		} else {
			expanded, err := expand(host)
			if err != nil {
				return Spec{}, err
			}
			host = expanded
		}
		spec.Host = host
		portText, err := expand(declared.Port)
		if err != nil {
			return Spec{}, err
		}
		port, convErr := strconv.Atoi(portText)
		if convErr != nil {
			return Spec{}, fmt.Errorf("health port %q is not a number", portText)
		}
		spec.Port = port
	}
	return spec, nil
}

// Defaults supplies the fallbacks for interval, timeout and retries.
type Defaults struct {
	Interval time.Duration
	Timeout  time.Duration
	Retries  int
}
