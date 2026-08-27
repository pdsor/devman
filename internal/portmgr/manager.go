// Package portmgr is the single place where ports are allocated.
//
// Every service start goes through Manager.ReserveService. Nothing else in
// DevMan is allowed to look for a free port, because a "find a free port and
// then bind it" helper used from several places is exactly how concurrent
// starts end up sharing one port.
//
// Exclusivity is guaranteed by the database's partial unique index on active
// allocations, so the winner of a race is decided by the storage layer rather
// than by an in-process lock.
package portmgr

import (
	"fmt"
	"sync"
	"time"

	"github.com/devman-project/devman/internal/settings"
	"github.com/devman-project/devman/internal/storage"
	"github.com/devman-project/devman/pkg/config"
	"github.com/devman-project/devman/pkg/dto"
	"github.com/devman-project/devman/pkg/errs"
)

// Prober answers "is this port usable right now" and, best effort, "who has
// it". It is an interface so tests can simulate an occupied machine.
type Prober interface {
	// Available reports whether DevMan could bind the port right now.
	Available(port int) bool
	// Owner identifies the external process holding a port. It returns nil when
	// the platform cannot tell, which must never block a start.
	Owner(port int) *dto.PortOwner
}

// Manager allocates and tracks ports.
type Manager struct {
	db     *storage.DB
	prober Prober

	// allocMu serialises range scans so two concurrent starts do not walk the
	// same window in lockstep. Correctness comes from the database index, not
	// from this lock.
	allocMu sync.Mutex

	// settingsMu guards the settings pointer, which the daemon may swap when
	// the user edits config.yaml. It is deliberately separate from allocMu so a
	// range lookup during an allocation cannot deadlock.
	settingsMu sync.RWMutex
	settings   *settings.Settings
}

// New creates a manager. Passing a nil prober uses the real OS prober.
func New(db *storage.DB, cfg *settings.Settings, prober Prober) *Manager {
	if prober == nil {
		prober = NewOSProber()
	}
	if cfg == nil {
		cfg = settings.Default()
	}
	return &Manager{db: db, prober: prober, settings: cfg}
}

// SetSettings swaps the settings used for range lookups.
func (m *Manager) SetSettings(cfg *settings.Settings) {
	m.settingsMu.Lock()
	defer m.settingsMu.Unlock()
	m.settings = cfg
}

func (m *Manager) currentSettings() *settings.Settings {
	m.settingsMu.RLock()
	defer m.settingsMu.RUnlock()
	return m.settings
}

// Allocation is the result of reserving every port of one service.
type Allocation struct {
	// ByName maps a declared port name to the allocated number.
	ByName map[string]int
	// Env maps environment variable names to the injected port values.
	Env map[string]string
	// Records are the persisted allocations, in declaration order.
	Records []storage.PortRecord
}

// Port returns the allocated number for a declared name.
func (a Allocation) Port(name string) (int, bool) {
	port, ok := a.ByName[name]
	return port, ok
}

// ReserveService allocates every port declared by a service.
//
// Fixed ports are never silently moved: if `value: 8000` is taken, the start
// fails with PORT_CONFLICT and names the holder. `value: auto` honours
// `preferred` when it happens to be free and otherwise takes the next free port
// in the declared range.
//
// On any failure, every port already reserved for this call is released, so a
// half-allocated service cannot leak ports.
func (m *Manager) ReserveService(projectID, service string, specs []config.PortSpec) (Allocation, error) {
	allocation := Allocation{
		ByName: map[string]int{},
		Env:    map[string]string{},
	}
	if len(specs) == 0 {
		return allocation, nil
	}

	// The scan is serialised so two concurrent starts do not walk the same range
	// in lockstep and collide on every candidate. Correctness still comes from
	// the database index, not from this lock.
	m.allocMu.Lock()
	defer m.allocMu.Unlock()

	rollback := func() {
		for _, record := range allocation.Records {
			_ = m.db.ReleasePort(record.ID)
		}
	}

	for _, spec := range specs {
		record, err := m.reserveOne(projectID, service, spec)
		if err != nil {
			rollback()
			return Allocation{}, err
		}
		allocation.Records = append(allocation.Records, record)
		allocation.ByName[spec.Name] = record.Port
		if spec.Env != "" {
			allocation.Env[spec.Env] = fmt.Sprint(record.Port)
		}
	}
	return allocation, nil
}

func (m *Manager) reserveOne(projectID, service string, spec config.PortSpec) (storage.PortRecord, error) {
	if !spec.Value.Auto {
		return m.reserveFixed(projectID, service, spec, spec.Value.Number)
	}
	if spec.Preferred != 0 {
		if record, err := m.tryReserve(projectID, service, spec, spec.Preferred); err == nil {
			return record, nil
		}
	}
	return m.reserveFromRange(projectID, service, spec)
}

func (m *Manager) reserveFixed(
	projectID, service string, spec config.PortSpec, port int,
) (storage.PortRecord, error) {
	record, err := m.tryReserve(projectID, service, spec, port)
	if err == nil {
		return record, nil
	}
	conflict := errs.From(err)
	if conflict.Code != errs.CodePortConflict {
		return storage.PortRecord{}, err
	}
	// Enrich the message so the GUI and the CLI can say who is holding it.
	if owner := m.prober.Owner(port); owner != nil {
		if owner.PID != 0 {
			conflict = conflict.With("owner_pid", owner.PID)
		}
		if owner.Name != "" {
			conflict = conflict.With("owner_process", owner.Name)
		}
	}
	return storage.PortRecord{}, conflict.
		With("port", port).
		With("port_name", spec.Name).
		With("fixed", true)
}

func (m *Manager) reserveFromRange(
	projectID, service string, spec config.PortSpec,
) (storage.PortRecord, error) {
	current := m.currentSettings()
	rangeName := spec.Range
	if rangeName == "" {
		rangeName = settings.RangeGeneral
	}
	if !current.HasRange(rangeName) {
		return storage.PortRecord{}, errs.New(errs.CodeConfigInvalid,
			"port range %q is not defined in the global settings", rangeName)
	}
	window := current.Range(rangeName)

	for port := window.Start; port <= window.End; port++ {
		record, err := m.tryReserve(projectID, service, spec, port)
		if err == nil {
			return record, nil
		}
		if !errs.Is(err, errs.CodePortConflict) {
			return storage.PortRecord{}, err
		}
	}
	return storage.PortRecord{}, errs.New(errs.CodePortExhausted,
		"no free port in range %q (%d-%d)", rangeName, window.Start, window.End).
		With("range", rangeName)
}

// tryReserve claims one specific port, checking both the registry and the OS.
func (m *Manager) tryReserve(
	projectID, service string, spec config.PortSpec, port int,
) (storage.PortRecord, error) {
	if port < 1 || port > 65535 {
		return storage.PortRecord{}, errs.New(errs.CodeConfigInvalid,
			"port %d is out of range 1-65535", port)
	}
	if !m.prober.Available(port) {
		return storage.PortRecord{}, errs.New(errs.CodePortConflict,
			"port %d is in use by another process", port)
	}
	// The insert is the actual claim; the OS probe above is only an optimisation
	// plus protection against non-DevMan listeners.
	return m.db.ReservePort(port, projectID, service, spec.Name, spec.Env)
}

// Verify checks whether the service actually bound the ports it was given and
// updates their state.
//
// A service that ignores its PORT variable is reported as UNVERIFIED rather
// than killed: the process is running, and if it matters the health check will
// fail on its own.
func (m *Manager) Verify(projectID, service string) ([]storage.PortRecord, error) {
	records, err := m.db.ServicePorts(projectID, service)
	if err != nil {
		return nil, err
	}
	for i, record := range records {
		state := storage.PortStateUnverified
		if !m.prober.Available(record.Port) {
			// Something is listening; since we hold the reservation, it is ours.
			state = storage.PortStateBound
		}
		if state != record.State {
			if err := m.db.SetPortState(record.ID, state); err != nil {
				return nil, err
			}
			records[i].State = state
		}
	}
	return records, nil
}

// ReleaseService frees every port held by a service.
func (m *Manager) ReleaseService(projectID, service string) error {
	return m.db.ReleaseServicePorts(projectID, service)
}

// ReleaseProject frees every port held by a project.
func (m *Manager) ReleaseProject(projectID string) error {
	return m.db.ReleaseProjectPorts(projectID)
}

// ReleaseAll frees every allocation. Reconciliation uses it before re-claiming
// the ports of processes that are genuinely still running.
func (m *Manager) ReleaseAll() error { return m.db.ReleaseAllPorts() }

// Adopt re-claims a specific port for a service during reconciliation, where
// the port is already bound by a surviving process.
func (m *Manager) Adopt(projectID, service, portName, envVar string, port int) (storage.PortRecord, error) {
	record, err := m.db.ReservePort(port, projectID, service, portName, envVar)
	if err != nil {
		return storage.PortRecord{}, err
	}
	state := storage.PortStateUnverified
	if !m.prober.Available(port) {
		state = storage.PortStateBound
	}
	if err := m.db.SetPortState(record.ID, state); err != nil {
		return record, err
	}
	record.State = state
	return record, nil
}

// ServicePorts returns the active allocations of a service as DTOs.
func (m *Manager) ServicePorts(projectID, service string) ([]dto.PortAllocation, error) {
	records, err := m.db.ServicePorts(projectID, service)
	if err != nil {
		return nil, err
	}
	return toDTOs(records), nil
}

// List returns every active allocation, ordered by port.
func (m *Manager) List() ([]dto.PortAllocation, error) {
	records, err := m.db.ActivePorts()
	if err != nil {
		return nil, err
	}
	return toDTOs(records), nil
}

// Usage describes one port: who DevMan thinks owns it, whether it is really
// occupied, and best-effort details about a foreign process holding it.
func (m *Manager) Usage(port int) (dto.PortUsage, error) {
	usage := dto.PortUsage{Port: port}
	record, found, err := m.db.ActivePort(port)
	if err != nil {
		return usage, err
	}
	if found {
		allocation := toDTO(record)
		usage.Allocation = &allocation
	}
	usage.Occupied = !m.prober.Available(port)
	if usage.Occupied && !found {
		usage.Owner = m.prober.Owner(port)
	}
	return usage, nil
}

func toDTOs(records []storage.PortRecord) []dto.PortAllocation {
	out := make([]dto.PortAllocation, 0, len(records))
	for _, record := range records {
		out = append(out, toDTO(record))
	}
	return out
}

func toDTO(record storage.PortRecord) dto.PortAllocation {
	return dto.PortAllocation{
		Port:        record.Port,
		Name:        record.PortName,
		Project:     record.ProjectID,
		Service:     record.ServiceName,
		EnvVar:      record.EnvVar,
		Status:      dto.PortStatus(record.State),
		AllocatedAt: record.AllocatedAt,
		ReleasedAt:  record.ReleasedAt,
	}
}

// WaitForBind polls until the service's ports are observed as bound or the
// timeout elapses. It reports whether every port was verified.
func (m *Manager) WaitForBind(projectID, service string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		records, err := m.Verify(projectID, service)
		if err != nil {
			return false, err
		}
		allBound := true
		for _, record := range records {
			if record.State != storage.PortStateBound {
				allBound = false
				break
			}
		}
		if allBound || time.Now().After(deadline) {
			return allBound, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}
