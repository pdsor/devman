package portmgr

import (
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/devman-project/devman/internal/settings"
	"github.com/devman-project/devman/internal/storage"
	"github.com/devman-project/devman/pkg/config"
	"github.com/devman-project/devman/pkg/dto"
	"github.com/devman-project/devman/pkg/errs"
)

// fakeProber simulates a machine where a fixed set of ports is taken by
// processes DevMan does not manage.
type fakeProber struct {
	mu       sync.Mutex
	occupied map[int]dto.PortOwner
	// bound lists ports where a listener is expected to appear, used to test
	// bind verification.
	bound map[int]bool
}

func newFakeProber(occupied ...int) *fakeProber {
	p := &fakeProber{occupied: map[int]dto.PortOwner{}, bound: map[int]bool{}}
	for _, port := range occupied {
		p.occupied[port] = dto.PortOwner{PID: 4512, Name: "python.exe"}
	}
	return p
}

func (p *fakeProber) Available(port int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, taken := p.occupied[port]; taken {
		return false
	}
	return !p.bound[port]
}

func (p *fakeProber) Listening(port int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, taken := p.occupied[port]; taken {
		return true
	}
	return p.bound[port]
}

func (p *fakeProber) Owner(port int) *dto.PortOwner {
	p.mu.Lock()
	defer p.mu.Unlock()
	if owner, taken := p.occupied[port]; taken {
		return &owner
	}
	return nil
}

func (p *fakeProber) markBound(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bound[port] = true
}

func newTestManager(t *testing.T, prober Prober) (*Manager, *storage.DB, string) {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "devman.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	root := t.TempDir()
	project := storage.ProjectRecord{
		ID:         storage.ProjectID(root),
		Name:       "app",
		Path:       root,
		ConfigPath: filepath.Join(root, "devman.yaml"),
	}
	if err := db.UpsertProject(project); err != nil {
		t.Fatal(err)
	}
	return New(db, settings.Default(), prober), db, project.ID
}

func autoPort(name, envVar, rangeName string, preferred int) config.PortSpec {
	return config.PortSpec{
		Name:      name,
		Value:     config.PortValue{Auto: true},
		Preferred: preferred,
		Env:       envVar,
		Range:     rangeName,
	}
}

func fixedPort(name, envVar string, port int) config.PortSpec {
	return config.PortSpec{
		Name:  name,
		Value: config.PortValue{Number: port},
		Env:   envVar,
		Range: settings.RangeGeneral,
	}
}

func TestPreferredPortIsUsedWhenFree(t *testing.T) {
	manager, _, project := newTestManager(t, newFakeProber())

	allocation, err := manager.ReserveService(project, "frontend",
		[]config.PortSpec{autoPort("http", "PORT", "frontend", 3000)})
	if err != nil {
		t.Fatalf("ReserveService: %v", err)
	}
	if got := allocation.ByName["http"]; got != 3000 {
		t.Fatalf("port = %d, want the preferred 3000", got)
	}
	if allocation.Env["PORT"] != "3000" {
		t.Fatalf("env injection = %+v", allocation.Env)
	}
}

func TestTwoProjectsPreferringTheSamePortGetNeighbours(t *testing.T) {
	// This is the headline port-manager requirement: two projects that both
	// prefer 3000 must start without either config being edited.
	manager, _, projectA := newTestManager(t, newFakeProber())
	rootB := t.TempDir()
	projectB := storage.ProjectID(rootB)
	if err := manager.db.UpsertProject(storage.ProjectRecord{
		ID: projectB, Name: "b", Path: rootB, ConfigPath: filepath.Join(rootB, "devman.yaml"),
	}); err != nil {
		t.Fatal(err)
	}

	first, err := manager.ReserveService(projectA, "frontend",
		[]config.PortSpec{autoPort("http", "PORT", "frontend", 3000)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.ReserveService(projectB, "frontend",
		[]config.PortSpec{autoPort("http", "PORT", "frontend", 3000)})
	if err != nil {
		t.Fatal(err)
	}
	if first.ByName["http"] != 3000 || second.ByName["http"] != 3001 {
		t.Fatalf("ports = %d and %d, want 3000 and 3001",
			first.ByName["http"], second.ByName["http"])
	}
}

func TestConcurrentAutoAllocationNeverSharesAPort(t *testing.T) {
	manager, _, project := newTestManager(t, newFakeProber())

	const services = 10
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		ports = map[int]string{}
	)
	for i := 0; i < services; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("svc%02d", i)
			allocation, err := manager.ReserveService(project, name,
				[]config.PortSpec{autoPort("http", "PORT", "backend", 8000)})
			if err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			port := allocation.ByName["http"]
			mu.Lock()
			defer mu.Unlock()
			if existing, clash := ports[port]; clash {
				t.Errorf("port %d handed to both %s and %s", port, existing, name)
			}
			ports[port] = name
		}(i)
	}
	wg.Wait()

	if len(ports) != services {
		t.Fatalf("allocated %d distinct ports for %d services", len(ports), services)
	}
	for port := range ports {
		if port < 8000 || port > 8999 {
			t.Fatalf("port %d outside the declared backend range", port)
		}
	}
}

func TestFixedPortConflictIsNeverSilentlyMoved(t *testing.T) {
	manager, _, project := newTestManager(t, newFakeProber(8000))

	_, err := manager.ReserveService(project, "backend",
		[]config.PortSpec{fixedPort("http", "PORT", 8000)})
	if !errs.Is(err, errs.CodePortConflict) {
		t.Fatalf("err = %v, want PORT_CONFLICT", err)
	}
	details := errs.From(err).Details
	if details["fixed"] != true {
		t.Fatalf("error must record that the port was fixed: %+v", details)
	}
	// The GUI needs to be able to say who holds it.
	if details["owner_pid"] != 4512 || details["owner_process"] != "python.exe" {
		t.Fatalf("owner details missing: %+v", details)
	}
	if active, err := manager.List(); err != nil || len(active) != 0 {
		t.Fatalf("a failed reservation must leave nothing behind: %+v, %v", active, err)
	}
}

func TestOccupiedPreferredPortFallsBackToRange(t *testing.T) {
	manager, _, project := newTestManager(t, newFakeProber(3000, 3001))

	allocation, err := manager.ReserveService(project, "frontend",
		[]config.PortSpec{autoPort("http", "PORT", "frontend", 3000)})
	if err != nil {
		t.Fatal(err)
	}
	if got := allocation.ByName["http"]; got != 3002 {
		t.Fatalf("port = %d, want 3002 (3000 and 3001 are taken externally)", got)
	}
}

func TestMultiPortServiceIsAllOrNothing(t *testing.T) {
	manager, _, project := newTestManager(t, newFakeProber(9229))

	allocation, err := manager.ReserveService(project, "backend", []config.PortSpec{
		autoPort("http", "PORT", "backend", 0),
		fixedPort("debug", "DEBUG_PORT", 9229),
	})
	if err == nil {
		t.Fatalf("expected the fixed debug port to fail, got %+v", allocation)
	}
	// The http port reserved before the failure must have been rolled back.
	active, err := manager.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("partial allocation leaked: %+v", active)
	}
}

func TestMultiPortServiceInjectsBothVariables(t *testing.T) {
	manager, _, project := newTestManager(t, newFakeProber())

	allocation, err := manager.ReserveService(project, "backend", []config.PortSpec{
		autoPort("http", "PORT", "backend", 0),
		autoPort("debug", "DEBUG_PORT", "general", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(allocation.ByName) != 2 {
		t.Fatalf("allocation = %+v", allocation.ByName)
	}
	if allocation.Env["PORT"] == "" || allocation.Env["DEBUG_PORT"] == "" {
		t.Fatalf("env = %+v", allocation.Env)
	}
	if allocation.ByName["http"] == allocation.ByName["debug"] {
		t.Fatal("two ports of one service must differ")
	}
	if port := allocation.ByName["debug"]; port < 10000 || port > 19999 {
		t.Fatalf("debug port %d outside the general range", port)
	}
}

func TestVerifyMarksBoundAndUnverified(t *testing.T) {
	prober := newFakeProber()
	manager, _, project := newTestManager(t, prober)

	allocation, err := manager.ReserveService(project, "backend", []config.PortSpec{
		autoPort("http", "PORT", "backend", 8000),
		autoPort("debug", "DEBUG_PORT", "backend", 8500),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The service listens on http but ignores the debug port.
	prober.markBound(allocation.ByName["http"])

	records, err := manager.Verify(project, "backend")
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, record := range records {
		states[record.PortName] = record.State
	}
	if states["http"] != storage.PortStateBound {
		t.Fatalf("http state = %q, want BOUND", states["http"])
	}
	// A service that never binds its port is reported, not killed.
	if states["debug"] != storage.PortStateUnverified {
		t.Fatalf("debug state = %q, want UNVERIFIED", states["debug"])
	}

	// A bound port must still be exclusive.
	other, err := manager.Usage(allocation.ByName["http"])
	if err != nil {
		t.Fatal(err)
	}
	if other.Allocation == nil || other.Allocation.Service != "backend" {
		t.Fatalf("usage = %+v", other)
	}
}

func TestReleaseFreesPortsForReuse(t *testing.T) {
	manager, _, project := newTestManager(t, newFakeProber())

	first, err := manager.ReserveService(project, "frontend",
		[]config.PortSpec{autoPort("http", "PORT", "frontend", 3000)})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ReleaseService(project, "frontend"); err != nil {
		t.Fatal(err)
	}
	if active, err := manager.List(); err != nil || len(active) != 0 {
		t.Fatalf("ports after release = %+v, %v", active, err)
	}

	// Restarting the service must be able to take the same port again.
	second, err := manager.ReserveService(project, "frontend",
		[]config.PortSpec{autoPort("http", "PORT", "frontend", 3000)})
	if err != nil {
		t.Fatal(err)
	}
	if second.ByName["http"] != first.ByName["http"] {
		t.Fatalf("restart got %d, want the released %d",
			second.ByName["http"], first.ByName["http"])
	}
}

func TestExhaustedRangeReportsClearly(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "devman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	root := t.TempDir()
	project := storage.ProjectID(root)
	if err := db.UpsertProject(storage.ProjectRecord{
		ID: project, Name: "tiny", Path: root, ConfigPath: filepath.Join(root, "devman.yaml"),
	}); err != nil {
		t.Fatal(err)
	}

	tiny := settings.Default()
	tiny.PortRanges["tiny"] = settings.PortRange{Start: 45000, End: 45001}
	manager := New(db, tiny, newFakeProber())

	for i := 0; i < 2; i++ {
		if _, err := manager.ReserveService(project, fmt.Sprintf("svc%d", i),
			[]config.PortSpec{autoPort("http", "PORT", "tiny", 0)}); err != nil {
			t.Fatalf("reservation %d: %v", i, err)
		}
	}
	_, err = manager.ReserveService(project, "overflow",
		[]config.PortSpec{autoPort("http", "PORT", "tiny", 0)})
	if !errs.Is(err, errs.CodePortExhausted) {
		t.Fatalf("err = %v, want PORT_EXHAUSTED", err)
	}
}

func TestUnknownRangeIsRejected(t *testing.T) {
	manager, _, project := newTestManager(t, newFakeProber())
	_, err := manager.ReserveService(project, "svc",
		[]config.PortSpec{autoPort("http", "PORT", "does-not-exist", 0)})
	if !errs.Is(err, errs.CodeConfigInvalid) {
		t.Fatalf("err = %v, want CONFIG_INVALID", err)
	}
}

func TestOSProberDetectsRealListener(t *testing.T) {
	prober := NewOSProber()

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind a loopback port: %v", err)
	}
	_, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)

	if prober.Available(port) {
		t.Fatalf("port %d is bound to 127.0.0.1 but reported as available", port)
	}
	if !IsListening("127.0.0.1", port, 0) {
		t.Fatalf("IsListening should see the listener on %d", port)
	}
	listener.Close()

	// After closing, the port must be usable again.
	if !prober.Available(port) {
		t.Fatalf("port %d still reported as occupied after close", port)
	}
}

func TestOSProberDetectsWildcardListener(t *testing.T) {
	prober := NewOSProber()

	// A service bound only to 0.0.0.0 must not look free to DevMan, otherwise a
	// 127.0.0.1-only probe would hand out a port that cannot actually be used.
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Skipf("cannot bind a wildcard port: %v", err)
	}
	defer listener.Close()
	_, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)

	if prober.Available(port) {
		t.Fatalf("port %d is bound to 0.0.0.0 but reported as available", port)
	}
}
