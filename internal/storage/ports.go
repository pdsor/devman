package storage

import (
	"database/sql"
	"errors"
	"time"

	"github.com/devman-project/devman/pkg/errs"
)

// Port allocation states. These mirror dto.PortStatus and are the values the
// partial unique index keys on, so they must not be renamed casually.
const (
	PortStateReserved   = "RESERVED"
	PortStateBound      = "BOUND"
	PortStateUnverified = "UNVERIFIED"
	PortStateReleased   = "RELEASED"
	PortStateConflict   = "CONFLICT"
)

// PortRecord is one port allocation.
type PortRecord struct {
	ID          int64
	Port        int
	ProjectID   string
	ServiceName string
	PortName    string
	EnvVar      string
	State       string
	AllocatedAt time.Time
	ReleasedAt  *time.Time
}

const portColumns = `id, port, project_id, service_name, port_name, env_var,
	state, allocated_at, released_at`

func scanPort(scanner interface{ Scan(...any) error }) (PortRecord, error) {
	var (
		record      PortRecord
		envVar      sql.NullString
		allocatedAt string
		releasedAt  sql.NullString
	)
	err := scanner.Scan(&record.ID, &record.Port, &record.ProjectID, &record.ServiceName,
		&record.PortName, &envVar, &record.State, &allocatedAt, &releasedAt)
	if err != nil {
		return PortRecord{}, err
	}
	record.EnvVar = envVar.String
	record.AllocatedAt = parseTime(allocatedAt)
	record.ReleasedAt = scanTime(releasedAt)
	return record, nil
}

// ReservePort claims a port for a service.
//
// Atomicity comes from the partial unique index on active allocations: if two
// callers race for the same port, exactly one INSERT succeeds and the other
// gets PORT_CONFLICT. No application-level lock is involved, so the guarantee
// also holds across daemon threads and, in principle, across processes.
func (db *DB) ReservePort(port int, projectID, service, portName, envVar string) (PortRecord, error) {
	now := time.Now().UTC()
	result, err := db.sql.Exec(`
		INSERT INTO port_allocations
			(port, project_id, service_name, port_name, env_var, state, allocated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		port, projectID, service, portName, nullString(envVar), PortStateReserved,
		formatTime(now))
	if err != nil {
		if isUniqueViolation(err) {
			holder, found, lookupErr := db.ActivePort(port)
			conflict := errs.New(errs.CodePortConflict, "port %d is already allocated", port)
			if lookupErr == nil && found {
				conflict = conflict.
					With("project", holder.ProjectID).
					With("service", holder.ServiceName)
			}
			return PortRecord{}, conflict
		}
		return PortRecord{}, errs.Wrap(errs.CodeInternal, err, "cannot reserve port %d", port)
	}
	id, _ := result.LastInsertId()
	return PortRecord{
		ID:          id,
		Port:        port,
		ProjectID:   projectID,
		ServiceName: service,
		PortName:    portName,
		EnvVar:      envVar,
		State:       PortStateReserved,
		AllocatedAt: now,
	}, nil
}

// SetPortState moves an active allocation to a new state.
func (db *DB) SetPortState(id int64, state string) error {
	_, err := db.sql.Exec(`UPDATE port_allocations SET state = ? WHERE id = ?`, state, id)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot update port state")
	}
	return nil
}

// ReleasePort marks one allocation released, freeing the port for reuse.
func (db *DB) ReleasePort(id int64) error {
	_, err := db.sql.Exec(
		`UPDATE port_allocations SET state = ?, released_at = ? WHERE id = ? AND released_at IS NULL`,
		PortStateReleased, formatTime(time.Now()), id)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot release port")
	}
	return nil
}

// ReleaseServicePorts releases every active port of one service.
func (db *DB) ReleaseServicePorts(projectID, service string) error {
	_, err := db.sql.Exec(`
		UPDATE port_allocations SET state = ?, released_at = ?
		WHERE project_id = ? AND service_name = ? AND released_at IS NULL`,
		PortStateReleased, formatTime(time.Now()), projectID, service)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot release service ports")
	}
	return nil
}

// ReleaseProjectPorts releases every active port of one project.
func (db *DB) ReleaseProjectPorts(projectID string) error {
	_, err := db.sql.Exec(`
		UPDATE port_allocations SET state = ?, released_at = ?
		WHERE project_id = ? AND released_at IS NULL`,
		PortStateReleased, formatTime(time.Now()), projectID)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot release project ports")
	}
	return nil
}

// ReleaseAllPorts releases every active allocation. Reconciliation uses this
// after deciding which services are genuinely still running.
func (db *DB) ReleaseAllPorts() error {
	_, err := db.sql.Exec(
		`UPDATE port_allocations SET state = ?, released_at = ? WHERE released_at IS NULL`,
		PortStateReleased, formatTime(time.Now()))
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot release ports")
	}
	return nil
}

const activeStates = `('RESERVED', 'BOUND', 'UNVERIFIED')`

// ActivePorts lists every allocation that currently holds a port.
func (db *DB) ActivePorts() ([]PortRecord, error) {
	rows, err := db.sql.Query(
		`SELECT ` + portColumns + ` FROM port_allocations
		 WHERE state IN ` + activeStates + ` ORDER BY port`)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "cannot list ports")
	}
	defer rows.Close()

	var out []PortRecord
	for rows.Next() {
		record, err := scanPort(rows)
		if err != nil {
			return nil, errs.Wrap(errs.CodeInternal, err, "cannot read port allocation")
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// ActivePort returns the active allocation for a port, if any.
func (db *DB) ActivePort(port int) (PortRecord, bool, error) {
	row := db.sql.QueryRow(
		`SELECT `+portColumns+` FROM port_allocations
		 WHERE port = ? AND state IN `+activeStates, port)
	record, err := scanPort(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PortRecord{}, false, nil
	}
	if err != nil {
		return PortRecord{}, false, errs.Wrap(errs.CodeInternal, err, "cannot read port allocation")
	}
	return record, true, nil
}

// ServicePorts lists the active allocations of one service.
func (db *DB) ServicePorts(projectID, service string) ([]PortRecord, error) {
	rows, err := db.sql.Query(
		`SELECT `+portColumns+` FROM port_allocations
		 WHERE project_id = ? AND service_name = ? AND state IN `+activeStates+`
		 ORDER BY port_name`, projectID, service)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "cannot list service ports")
	}
	defer rows.Close()

	var out []PortRecord
	for rows.Next() {
		record, err := scanPort(rows)
		if err != nil {
			return nil, errs.Wrap(errs.CodeInternal, err, "cannot read port allocation")
		}
		out = append(out, record)
	}
	return out, rows.Err()
}
