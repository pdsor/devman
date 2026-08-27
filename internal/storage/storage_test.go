package storage

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/devman-project/devman/pkg/errs"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "devman.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func seedProject(t *testing.T, db *DB, path string) ProjectRecord {
	t.Helper()
	record := ProjectRecord{
		ID:         ProjectID(path),
		Name:       filepath.Base(path),
		Path:       path,
		ConfigPath: filepath.Join(path, "devman.yaml"),
	}
	if err := db.UpsertProject(record); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	stored, err := db.Project(record.ID)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	return stored
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devman.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if version, err := first.GetMeta("schema_version"); err != nil || version != "1" {
		t.Fatalf("schema_version = %q, %v", version, err)
	}
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer second.Close()
	if _, err := second.Projects(); err != nil {
		t.Fatalf("Projects after reopen: %v", err)
	}
}

func TestProjectIDIsDeterministic(t *testing.T) {
	a := ProjectID(filepath.Join("some", "project"))
	if a != ProjectID(filepath.Join("some", "project")) {
		t.Fatal("ProjectID must be stable so re-registering keeps logs and history")
	}
	if a == ProjectID(filepath.Join("other", "project")) {
		t.Fatal("different paths must produce different ids")
	}
}

func TestProjectCRUD(t *testing.T) {
	db := openTestDB(t)
	project := seedProject(t, db, filepath.Join(t.TempDir(), "crm"))

	if project.Favorite {
		t.Fatal("favourite must default to false")
	}
	if project.TrustedFingerprint != "" {
		t.Fatal("a new project must not be trusted")
	}

	if err := db.SetProjectTrust(project.ID, "fingerprint-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectFavorite(project.ID, true); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Truncate(time.Millisecond)
	if err := db.TouchProjectStarted(project.ID, started); err != nil {
		t.Fatal(err)
	}

	reloaded, err := db.Project(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.TrustedFingerprint != "fingerprint-1" || !reloaded.Favorite {
		t.Fatalf("project = %+v", reloaded)
	}
	if reloaded.LastStartedAt == nil {
		t.Fatal("last_started_at not recorded")
	}

	byName, err := db.ProjectByName(project.Name)
	if err != nil || byName.ID != project.ID {
		t.Fatalf("ProjectByName = %+v, %v", byName, err)
	}
	byPath, err := db.ProjectByPath(project.Path)
	if err != nil || byPath.ID != project.ID {
		t.Fatalf("ProjectByPath = %+v, %v", byPath, err)
	}

	if err := db.DeleteProject(project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Project(project.ID); !errs.Is(err, errs.CodeProjectNotFound) {
		t.Fatalf("after delete err = %v", err)
	}
	if err := db.DeleteProject(project.ID); !errs.Is(err, errs.CodeProjectNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
}

func TestUpsertProjectRejectsDuplicatePath(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	seedProject(t, db, dir)

	// A different id claiming the same path must be refused: the path is the
	// project's identity.
	clash := ProjectRecord{
		ID:         "p_manual",
		Name:       "clash",
		Path:       dir,
		ConfigPath: filepath.Join(dir, "devman.yaml"),
	}
	if err := db.UpsertProject(clash); !errs.Is(err, errs.CodeProjectExists) {
		t.Fatalf("err = %v, want PROJECT_EXISTS", err)
	}
}

func TestServiceRuntimeRoundTrip(t *testing.T) {
	db := openTestDB(t)
	project := seedProject(t, db, filepath.Join(t.TempDir(), "app"))

	spawned := time.Now().UTC().Truncate(time.Millisecond)
	exit := 1
	record := ServiceRuntimeRecord{
		ProjectID:          project.ID,
		ServiceName:        "backend",
		DesiredState:       "RUNNING",
		ActualState:        "CRASHED",
		PID:                4321,
		SpawnedAt:          &spawned,
		Executable:         "/usr/bin/uv",
		CommandFingerprint: "abc123",
		InstanceID:         "inst-1",
		RestartCount:       2,
		LastExitCode:       &exit,
		LogCapture:         "detached",
		Adopted:            true,
	}
	if err := db.UpsertServiceRuntime(record); err != nil {
		t.Fatal(err)
	}

	stored, err := db.ServiceRuntime(project.ID, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if stored.DesiredState != "RUNNING" || stored.ActualState != "CRASHED" {
		t.Fatalf("states = %+v", stored)
	}
	if stored.PID != 4321 || stored.RestartCount != 2 || !stored.Adopted {
		t.Fatalf("record = %+v", stored)
	}
	if stored.LastExitCode == nil || *stored.LastExitCode != 1 {
		t.Fatalf("exit code = %v", stored.LastExitCode)
	}
	if stored.SpawnedAt == nil || !stored.SpawnedAt.Equal(spawned) {
		t.Fatalf("spawned_at = %v, want %v", stored.SpawnedAt, spawned)
	}
	if stored.LogCapture != "detached" {
		t.Fatalf("log_capture = %q", stored.LogCapture)
	}

	// Upsert must update, not duplicate.
	record.ActualState = "RUNNING"
	record.LastExitCode = nil
	if err := db.UpsertServiceRuntime(record); err != nil {
		t.Fatal(err)
	}
	all, err := db.ServiceRuntimes(project.ID)
	if err != nil || len(all) != 1 || all[0].ActualState != "RUNNING" {
		t.Fatalf("runtimes = %+v, %v", all, err)
	}
	if all[0].LastExitCode != nil {
		t.Fatal("exit code must be clearable")
	}

	if _, err := db.ServiceRuntime(project.ID, "missing"); !errs.Is(err, errs.CodeServiceNotFound) {
		t.Fatalf("missing service err = %v", err)
	}
}

func TestDeleteProjectCascadesRuntimeState(t *testing.T) {
	db := openTestDB(t)
	project := seedProject(t, db, filepath.Join(t.TempDir(), "app"))
	if err := db.UpsertServiceRuntime(ServiceRuntimeRecord{
		ProjectID: project.ID, ServiceName: "web",
		DesiredState: "STOPPED", ActualState: "STOPPED",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReservePort(3000, project.ID, "web", "http", "PORT"); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteProject(project.ID); err != nil {
		t.Fatal(err)
	}
	if runtimes, err := db.ServiceRuntimes(project.ID); err != nil || len(runtimes) != 0 {
		t.Fatalf("runtime state survived deletion: %+v, %v", runtimes, err)
	}
	// The port must be free again, otherwise unregistering a project would leak
	// its ports until the daemon restarted.
	active, err := db.ActivePorts()
	if err != nil || len(active) != 0 {
		t.Fatalf("ports after deletion = %+v, %v", active, err)
	}
}

func TestProcessInstanceLifecycle(t *testing.T) {
	db := openTestDB(t)
	project := seedProject(t, db, filepath.Join(t.TempDir(), "app"))

	if err := db.InsertInstance(InstanceRecord{
		ID: "inst-1", ProjectID: project.ID, ServiceName: "web", PID: 100,
		Status: "RUNNING", Runtime: "host", CommandLine: "pnpm dev", CWD: "/app/web",
	}); err != nil {
		t.Fatal(err)
	}
	exit := 0
	if err := db.FinishInstance("inst-1", "STOPPED", &exit, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertInstance(InstanceRecord{
		ID: "inst-2", ProjectID: project.ID, ServiceName: "web", PID: 101,
		Status: "RUNNING", Runtime: "host", RestartCount: 1,
	}); err != nil {
		t.Fatal(err)
	}

	instances, err := db.Instances(project.ID, "web", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 {
		t.Fatalf("instances = %d", len(instances))
	}
	// Newest first.
	if instances[0].ID != "inst-2" {
		t.Fatalf("order = %+v", instances)
	}
	finished := instances[1]
	if finished.Status != "STOPPED" || finished.StoppedAt == nil ||
		finished.ExitCode == nil || *finished.ExitCode != 0 {
		t.Fatalf("finished instance = %+v", finished)
	}
	if finished.CommandLine != "pnpm dev" {
		t.Fatalf("command line = %q", finished.CommandLine)
	}
}

func TestPortReservationIsExclusive(t *testing.T) {
	db := openTestDB(t)
	a := seedProject(t, db, filepath.Join(t.TempDir(), "a"))
	b := seedProject(t, db, filepath.Join(t.TempDir(), "b"))

	first, err := db.ReservePort(3000, a.ID, "frontend", "http", "PORT")
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	_, err = db.ReservePort(3000, b.ID, "frontend", "http", "PORT")
	if !errs.Is(err, errs.CodePortConflict) {
		t.Fatalf("second reserve err = %v, want PORT_CONFLICT", err)
	}
	if details := errs.From(err).Details; details["service"] != "frontend" {
		t.Fatalf("conflict must name the holder, got %+v", details)
	}

	// Released ports become available again.
	if err := db.ReleasePort(first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReservePort(3000, b.ID, "frontend", "http", "PORT"); err != nil {
		t.Fatalf("reserve after release: %v", err)
	}
}

func TestPortReservationRaceYieldsSingleWinner(t *testing.T) {
	db := openTestDB(t)
	project := seedProject(t, db, filepath.Join(t.TempDir(), "race"))

	const attempts = 10
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		conflicts int
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := db.ReservePort(4100, project.ID, fmt.Sprintf("svc%d", i), "http", "PORT")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errs.Is(err, errs.CodePortConflict):
				conflicts++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if succeeded != 1 || conflicts != attempts-1 {
		t.Fatalf("concurrent reservations: %d winners, %d conflicts (want 1 and %d)",
			succeeded, conflicts, attempts-1)
	}
}

func TestPortStateTransitionsAndBulkRelease(t *testing.T) {
	db := openTestDB(t)
	project := seedProject(t, db, filepath.Join(t.TempDir(), "app"))

	http, err := db.ReservePort(8000, project.ID, "backend", "http", "PORT")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReservePort(9229, project.ID, "backend", "debug", "DEBUG_PORT"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReservePort(3000, project.ID, "frontend", "http", "PORT"); err != nil {
		t.Fatal(err)
	}

	if err := db.SetPortState(http.ID, PortStateBound); err != nil {
		t.Fatal(err)
	}
	holder, found, err := db.ActivePort(8000)
	if err != nil || !found || holder.State != PortStateBound {
		t.Fatalf("ActivePort = %+v, %v, %v", holder, found, err)
	}
	// A bound port is still exclusive.
	if _, err := db.ReservePort(8000, project.ID, "other", "http", ""); !errs.Is(err, errs.CodePortConflict) {
		t.Fatalf("bound port must stay exclusive, got %v", err)
	}

	if ports, err := db.ServicePorts(project.ID, "backend"); err != nil || len(ports) != 2 {
		t.Fatalf("service ports = %+v, %v", ports, err)
	}
	if err := db.ReleaseServicePorts(project.ID, "backend"); err != nil {
		t.Fatal(err)
	}
	if ports, err := db.ServicePorts(project.ID, "backend"); err != nil || len(ports) != 0 {
		t.Fatalf("service ports after release = %+v, %v", ports, err)
	}
	// The other service keeps its port.
	if active, err := db.ActivePorts(); err != nil || len(active) != 1 || active[0].Port != 3000 {
		t.Fatalf("active ports = %+v, %v", active, err)
	}

	if err := db.ReleaseAllPorts(); err != nil {
		t.Fatal(err)
	}
	if active, err := db.ActivePorts(); err != nil || len(active) != 0 {
		t.Fatalf("active ports after release all = %+v, %v", active, err)
	}
	if _, found, err := db.ActivePort(3000); err != nil || found {
		t.Fatalf("released port still active: %v, %v", found, err)
	}
}

func TestEventsAppendAndPrune(t *testing.T) {
	db := openTestDB(t)
	project := seedProject(t, db, filepath.Join(t.TempDir(), "app"))

	for i := 0; i < 5; i++ {
		seq, err := db.InsertEvent(EventRecord{
			Type: "SERVICE_STARTED", ProjectID: project.ID, ServiceName: "web",
			Message: fmt.Sprintf("start %d", i),
			Data:    map[string]any{"attempt": i},
		})
		if err != nil {
			t.Fatal(err)
		}
		if seq == 0 {
			t.Fatal("event sequence must be assigned")
		}
	}

	events, err := db.Events(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 {
		t.Fatalf("events = %d", len(events))
	}
	if events[0].Message != "start 4" {
		t.Fatalf("events must be newest first: %+v", events[0])
	}
	if events[0].Data["attempt"] != float64(4) {
		t.Fatalf("event data lost: %+v", events[0].Data)
	}

	if err := db.PruneEvents(2); err != nil {
		t.Fatal(err)
	}
	remaining, err := db.Events(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Fatalf("events after prune = %d", len(remaining))
	}
}
