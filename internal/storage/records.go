package storage

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/devman-project/devman/pkg/errs"
)

// ProjectRecord is a registered project.
type ProjectRecord struct {
	ID         string
	Name       string
	Path       string
	ConfigPath string
	// TrustedFingerprint is the execution fingerprint the user approved. It is
	// empty for an untrusted project.
	TrustedFingerprint string
	Favorite           bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
	LastStartedAt      *time.Time
}

// ProjectID derives a stable id from the project path. It is deterministic so
// re-registering the same directory keeps its logs and history.
func ProjectID(path string) string {
	normalised := filepath.Clean(path)
	if filepath.Separator == '\\' {
		normalised = strings.ToLower(normalised)
	}
	sum := sha256.Sum256([]byte(normalised))
	return "p_" + hex.EncodeToString(sum[:])[:12]
}

// UpsertProject inserts or updates a project by id.
func (db *DB) UpsertProject(record ProjectRecord) error {
	now := time.Now().UTC()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	record.UpdatedAt = now
	_, err := db.sql.Exec(`
		INSERT INTO projects (id, name, path, config_path, trusted_fingerprint,
		                      favorite, created_at, updated_at, last_started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			path = excluded.path,
			config_path = excluded.config_path,
			trusted_fingerprint = excluded.trusted_fingerprint,
			favorite = excluded.favorite,
			updated_at = excluded.updated_at`,
		record.ID, record.Name, record.Path, record.ConfigPath,
		nullString(record.TrustedFingerprint), record.Favorite,
		formatTime(record.CreatedAt), formatTime(record.UpdatedAt),
		nullTime(record.LastStartedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return errs.New(errs.CodeProjectExists,
				"another project is already registered at %s", record.Path)
		}
		return errs.Wrap(errs.CodeInternal, err, "cannot save project")
	}
	return nil
}

const projectColumns = `id, name, path, config_path, trusted_fingerprint,
	favorite, created_at, updated_at, last_started_at`

func scanProject(scanner interface{ Scan(...any) error }) (ProjectRecord, error) {
	var (
		record      ProjectRecord
		fingerprint sql.NullString
		created     string
		updated     string
		lastStarted sql.NullString
	)
	err := scanner.Scan(&record.ID, &record.Name, &record.Path, &record.ConfigPath,
		&fingerprint, &record.Favorite, &created, &updated, &lastStarted)
	if err != nil {
		return ProjectRecord{}, err
	}
	record.TrustedFingerprint = fingerprint.String
	record.CreatedAt = parseTime(created)
	record.UpdatedAt = parseTime(updated)
	record.LastStartedAt = scanTime(lastStarted)
	return record, nil
}

// Project looks up a project by id.
func (db *DB) Project(id string) (ProjectRecord, error) {
	row := db.sql.QueryRow(`SELECT `+projectColumns+` FROM projects WHERE id = ?`, id)
	record, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectRecord{}, errs.New(errs.CodeProjectNotFound, "no project with id %q", id)
	}
	if err != nil {
		return ProjectRecord{}, errs.Wrap(errs.CodeInternal, err, "cannot read project")
	}
	return record, nil
}

// ProjectByPath looks up a project by filesystem path.
func (db *DB) ProjectByPath(path string) (ProjectRecord, error) {
	return db.Project(ProjectID(path))
}

// ProjectByName looks up a project by its declared name. Names are not unique
// in principle, so the oldest registration wins and callers that need
// certainty should use the id.
func (db *DB) ProjectByName(name string) (ProjectRecord, error) {
	row := db.sql.QueryRow(
		`SELECT `+projectColumns+` FROM projects WHERE name = ? ORDER BY created_at LIMIT 1`, name)
	record, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectRecord{}, errs.New(errs.CodeProjectNotFound, "no project named %q", name)
	}
	if err != nil {
		return ProjectRecord{}, errs.Wrap(errs.CodeInternal, err, "cannot read project")
	}
	return record, nil
}

// Projects lists every registered project, newest registration last.
func (db *DB) Projects() ([]ProjectRecord, error) {
	rows, err := db.sql.Query(`SELECT ` + projectColumns + ` FROM projects ORDER BY name, created_at`)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "cannot list projects")
	}
	defer rows.Close()

	var out []ProjectRecord
	for rows.Next() {
		record, err := scanProject(rows)
		if err != nil {
			return nil, errs.Wrap(errs.CodeInternal, err, "cannot read project")
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// DeleteProject removes a project and, by cascade, its runtime state.
func (db *DB) DeleteProject(id string) error {
	result, err := db.sql.Exec(`DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot delete project")
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return errs.New(errs.CodeProjectNotFound, "no project with id %q", id)
	}
	// Port allocations have no foreign key so that history survives a project
	// being unregistered while ports were still held; release them explicitly.
	if _, err := db.sql.Exec(
		`UPDATE port_allocations SET state = 'RELEASED', released_at = ?
		 WHERE project_id = ? AND released_at IS NULL`,
		formatTime(time.Now()), id); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot release ports of deleted project")
	}
	return nil
}

// SetProjectTrust stores (or clears) the approved execution fingerprint.
func (db *DB) SetProjectTrust(id, fingerprint string) error {
	_, err := db.sql.Exec(
		`UPDATE projects SET trusted_fingerprint = ?, updated_at = ? WHERE id = ?`,
		nullString(fingerprint), formatTime(time.Now()), id)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot update project trust")
	}
	return nil
}

// SetProjectFavorite toggles the favourite flag.
func (db *DB) SetProjectFavorite(id string, favorite bool) error {
	_, err := db.sql.Exec(
		`UPDATE projects SET favorite = ?, updated_at = ? WHERE id = ?`,
		favorite, formatTime(time.Now()), id)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot update favourite")
	}
	return nil
}

// TouchProjectStarted records the last time a project was started.
func (db *DB) TouchProjectStarted(id string, when time.Time) error {
	_, err := db.sql.Exec(
		`UPDATE projects SET last_started_at = ?, updated_at = ? WHERE id = ?`,
		formatTime(when), formatTime(time.Now()), id)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot update project start time")
	}
	return nil
}

// ServiceRuntimeRecord is the persisted runtime state of one service.
//
// Desired and actual state are stored separately from the first version so a
// manual stop can never be undone by a restart policy after a daemon restart.
type ServiceRuntimeRecord struct {
	ProjectID          string
	ServiceName        string
	DesiredState       string
	ActualState        string
	PID                int
	SpawnedAt          *time.Time
	Executable         string
	CommandFingerprint string
	InstanceID         string
	RestartCount       int
	LastExitCode       *int
	LogCapture         string
	Adopted            bool
	UpdatedAt          time.Time
}

// UpsertServiceRuntime writes the runtime state of one service.
func (db *DB) UpsertServiceRuntime(record ServiceRuntimeRecord) error {
	if record.LogCapture == "" {
		record.LogCapture = "none"
	}
	record.UpdatedAt = time.Now().UTC()
	_, err := db.sql.Exec(`
		INSERT INTO service_runtime (project_id, service_name, desired_state, actual_state,
			pid, spawned_at, executable, command_fingerprint, instance_id,
			restart_count, last_exit_code, log_capture, adopted, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, service_name) DO UPDATE SET
			desired_state = excluded.desired_state,
			actual_state = excluded.actual_state,
			pid = excluded.pid,
			spawned_at = excluded.spawned_at,
			executable = excluded.executable,
			command_fingerprint = excluded.command_fingerprint,
			instance_id = excluded.instance_id,
			restart_count = excluded.restart_count,
			last_exit_code = excluded.last_exit_code,
			log_capture = excluded.log_capture,
			adopted = excluded.adopted,
			updated_at = excluded.updated_at`,
		record.ProjectID, record.ServiceName, record.DesiredState, record.ActualState,
		record.PID, nullTime(record.SpawnedAt), nullString(record.Executable),
		nullString(record.CommandFingerprint), nullString(record.InstanceID),
		record.RestartCount, nullInt(record.LastExitCode), record.LogCapture,
		record.Adopted, formatTime(record.UpdatedAt))
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot save service runtime state")
	}
	return nil
}

const serviceRuntimeColumns = `project_id, service_name, desired_state, actual_state,
	pid, spawned_at, executable, command_fingerprint, instance_id,
	restart_count, last_exit_code, log_capture, adopted, updated_at`

func scanServiceRuntime(scanner interface{ Scan(...any) error }) (ServiceRuntimeRecord, error) {
	var (
		record      ServiceRuntimeRecord
		pid         sql.NullInt64
		spawnedAt   sql.NullString
		executable  sql.NullString
		fingerprint sql.NullString
		instanceID  sql.NullString
		exitCode    sql.NullInt64
		updatedAt   string
	)
	err := scanner.Scan(&record.ProjectID, &record.ServiceName, &record.DesiredState,
		&record.ActualState, &pid, &spawnedAt, &executable, &fingerprint, &instanceID,
		&record.RestartCount, &exitCode, &record.LogCapture, &record.Adopted, &updatedAt)
	if err != nil {
		return ServiceRuntimeRecord{}, err
	}
	record.PID = int(pid.Int64)
	record.SpawnedAt = scanTime(spawnedAt)
	record.Executable = executable.String
	record.CommandFingerprint = fingerprint.String
	record.InstanceID = instanceID.String
	record.LastExitCode = scanInt(exitCode)
	record.UpdatedAt = parseTime(updatedAt)
	return record, nil
}

// ServiceRuntime reads one service's runtime state. A missing row is reported
// as SERVICE_NOT_FOUND so callers can treat it as "never started".
func (db *DB) ServiceRuntime(projectID, service string) (ServiceRuntimeRecord, error) {
	row := db.sql.QueryRow(
		`SELECT `+serviceRuntimeColumns+` FROM service_runtime
		 WHERE project_id = ? AND service_name = ?`, projectID, service)
	record, err := scanServiceRuntime(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ServiceRuntimeRecord{}, errs.New(errs.CodeServiceNotFound,
			"no runtime state for %s/%s", projectID, service)
	}
	if err != nil {
		return ServiceRuntimeRecord{}, errs.Wrap(errs.CodeInternal, err, "cannot read runtime state")
	}
	return record, nil
}

// ServiceRuntimes lists runtime state, optionally filtered to one project.
func (db *DB) ServiceRuntimes(projectID string) ([]ServiceRuntimeRecord, error) {
	query := `SELECT ` + serviceRuntimeColumns + ` FROM service_runtime`
	var args []any
	if projectID != "" {
		query += ` WHERE project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY project_id, service_name`

	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "cannot list runtime state")
	}
	defer rows.Close()

	var out []ServiceRuntimeRecord
	for rows.Next() {
		record, err := scanServiceRuntime(rows)
		if err != nil {
			return nil, errs.Wrap(errs.CodeInternal, err, "cannot read runtime state")
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// InstanceRecord is one run of a service.
type InstanceRecord struct {
	ID           string
	ProjectID    string
	ServiceName  string
	PID          int
	Status       string
	Runtime      string
	CommandLine  string
	CWD          string
	StartedAt    time.Time
	StoppedAt    *time.Time
	ExitCode     *int
	RestartCount int
}

// InsertInstance records the start of a run.
func (db *DB) InsertInstance(record InstanceRecord) error {
	if record.StartedAt.IsZero() {
		record.StartedAt = time.Now().UTC()
	}
	_, err := db.sql.Exec(`
		INSERT INTO process_instances (id, project_id, service_name, pid, status, runtime,
			command_line, cwd, started_at, stopped_at, exit_code, restart_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.ProjectID, record.ServiceName, record.PID, record.Status,
		record.Runtime, nullString(record.CommandLine), nullString(record.CWD),
		formatTime(record.StartedAt), nullTime(record.StoppedAt),
		nullInt(record.ExitCode), record.RestartCount)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot record process instance")
	}
	return nil
}

// FinishInstance records the end of a run.
func (db *DB) FinishInstance(id, status string, exitCode *int, stoppedAt time.Time) error {
	_, err := db.sql.Exec(
		`UPDATE process_instances SET status = ?, exit_code = ?, stopped_at = ? WHERE id = ?`,
		status, nullInt(exitCode), formatTime(stoppedAt), id)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot finish process instance")
	}
	return nil
}

// Instances lists the most recent runs of a service, newest first.
func (db *DB) Instances(projectID, service string, limit int) ([]InstanceRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.sql.Query(`
		SELECT id, project_id, service_name, pid, status, runtime, command_line, cwd,
		       started_at, stopped_at, exit_code, restart_count
		FROM process_instances
		WHERE project_id = ? AND service_name = ?
		ORDER BY started_at DESC LIMIT ?`, projectID, service, limit)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "cannot list process instances")
	}
	defer rows.Close()

	var out []InstanceRecord
	for rows.Next() {
		var (
			record      InstanceRecord
			commandLine sql.NullString
			cwd         sql.NullString
			startedAt   string
			stoppedAt   sql.NullString
			exitCode    sql.NullInt64
		)
		if err := rows.Scan(&record.ID, &record.ProjectID, &record.ServiceName, &record.PID,
			&record.Status, &record.Runtime, &commandLine, &cwd, &startedAt, &stoppedAt,
			&exitCode, &record.RestartCount); err != nil {
			return nil, errs.Wrap(errs.CodeInternal, err, "cannot read process instance")
		}
		record.CommandLine = commandLine.String
		record.CWD = cwd.String
		record.StartedAt = parseTime(startedAt)
		record.StoppedAt = scanTime(stoppedAt)
		record.ExitCode = scanInt(exitCode)
		out = append(out, record)
	}
	return out, rows.Err()
}

// EventRecord is a persisted daemon event.
type EventRecord struct {
	Seq         uint64
	Type        string
	ProjectID   string
	ServiceName string
	Message     string
	Data        map[string]any
	CreatedAt   time.Time
}

// InsertEvent appends an event and returns its sequence number.
func (db *DB) InsertEvent(record EventRecord) (uint64, error) {
	var payload any
	if len(record.Data) > 0 {
		encoded, err := json.Marshal(record.Data)
		if err == nil {
			payload = string(encoded)
		}
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	result, err := db.sql.Exec(`
		INSERT INTO events (type, project_id, service_name, message, data, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		record.Type, nullString(record.ProjectID), nullString(record.ServiceName),
		nullString(record.Message), payload, formatTime(record.CreatedAt))
	if err != nil {
		return 0, errs.Wrap(errs.CodeInternal, err, "cannot record event")
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return 0, nil
	}
	return uint64(seq), nil
}

// Events returns recent events, newest first.
func (db *DB) Events(limit int) ([]EventRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.sql.Query(`
		SELECT seq, type, project_id, service_name, message, data, created_at
		FROM events ORDER BY seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "cannot list events")
	}
	defer rows.Close()

	var out []EventRecord
	for rows.Next() {
		var (
			record    EventRecord
			seq       int64
			projectID sql.NullString
			service   sql.NullString
			message   sql.NullString
			data      sql.NullString
			createdAt string
		)
		if err := rows.Scan(&seq, &record.Type, &projectID, &service, &message, &data,
			&createdAt); err != nil {
			return nil, errs.Wrap(errs.CodeInternal, err, "cannot read event")
		}
		record.Seq = uint64(seq)
		record.ProjectID = projectID.String
		record.ServiceName = service.String
		record.Message = message.String
		record.CreatedAt = parseTime(createdAt)
		if data.Valid && data.String != "" {
			_ = json.Unmarshal([]byte(data.String), &record.Data)
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// PruneEvents keeps only the newest keep events.
func (db *DB) PruneEvents(keep int) error {
	if keep <= 0 {
		return nil
	}
	_, err := db.sql.Exec(`
		DELETE FROM events WHERE seq <= (
			SELECT seq FROM events ORDER BY seq DESC LIMIT 1 OFFSET ?
		)`, keep)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot prune events")
	}
	return nil
}
