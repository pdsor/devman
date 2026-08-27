package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/devman-project/devman/internal/events"
	"github.com/devman-project/devman/internal/logstore"
	"github.com/devman-project/devman/internal/paths"
	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/internal/portmgr"
	"github.com/devman-project/devman/internal/registry"
	"github.com/devman-project/devman/internal/runtime"
	"github.com/devman-project/devman/internal/settings"
	"github.com/devman-project/devman/internal/storage"
	"github.com/devman-project/devman/internal/supervisor"
	"github.com/devman-project/devman/pkg/dto"
	"github.com/devman-project/devman/pkg/errs"
)

// Run starts the daemon in the current process and blocks until it is asked to
// stop, either through the API or by a signal.
//
// Everything the daemon owns is created here, in one place, so the ownership and
// shutdown order are obvious: the database and log store outlive the supervisor,
// and the supervisor stops services before the API stops answering.
func Run(layout paths.Layout, version string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}

	// On Windows a console is what makes CTRL_BREAK deliverable to a service's
	// process group. Without it every stop would be a force kill.
	if err := platform.EnsureConsole(); err != nil {
		fmt.Fprintf(out, "warning: graceful shutdown is unavailable: %v\n", err)
	}

	cfg, err := settings.Load(layout.Settings)
	if err != nil {
		return err
	}
	if err := layout.EnsureDirs(); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot create the data directory")
	}
	// Writing the file on first run makes the defaults discoverable and editable.
	if _, statErr := os.Stat(layout.Settings); os.IsNotExist(statErr) {
		if saveErr := cfg.Save(layout.Settings); saveErr != nil {
			return saveErr
		}
	}

	db, err := storage.Open(layout.Database)
	if err != nil {
		return err
	}
	defer db.Close()

	listener, err := Bind(layout, cfg, version)
	if err != nil {
		return err
	}

	logs := logstore.NewManager(layout.Logs, logstore.Options{
		MaxSizeBytes: int64(cfg.Logs.MaxSizeMB) * 1024 * 1024,
		MaxBackups:   cfg.Logs.MaxBackups,
		RingCapacity: cfg.Logs.RingBuffer,
	})
	defer logs.Close()

	reg := registry.New(db)
	ports := portmgr.New(db, cfg, nil)

	// Events are persisted as they are published, so `devman logs`-style history
	// survives a daemon restart and the GUI can catch up after reconnecting.
	bus := events.New(func(event dto.Event) {
		_, _ = db.InsertEvent(storage.EventRecord{
			Type:        string(event.Type),
			ProjectID:   event.Project,
			ServiceName: event.Service,
			Message:     event.Message,
			Data:        event.Data,
			CreatedAt:   event.Timestamp,
		})
	})
	defer bus.Close()

	sup := supervisor.New(supervisor.Deps{
		DB:       db,
		Registry: reg,
		Ports:    ports,
		Logs:     logs,
		Events:   bus,
		Runtimes: runtime.NewSet(),
		Settings: func() *settings.Settings { return cfg },
	})
	defer sup.Close()

	server := NewServer(listener, Options{
		Layout:     layout,
		Settings:   cfg,
		DB:         db,
		Registry:   reg,
		Ports:      ports,
		Logs:       logs,
		Events:     bus,
		Supervisor: sup,
		Version:    version,
	})

	// Reconciliation happens before the API accepts traffic, so the first status
	// call already reflects reality rather than a stale record.
	reconciled, err := sup.Reconcile()
	if err != nil {
		fmt.Fprintf(out, "warning: reconciliation failed: %v\n", err)
	} else {
		for _, adopted := range reconciled.Adopted {
			fmt.Fprintf(out, "adopted %s/%s (pid %d, log capture detached)\n",
				adopted.Project, adopted.Name, adopted.PID)
		}
		for _, vanished := range reconciled.Vanished {
			fmt.Fprintf(out, "%s/%s was gone; ports released\n", vanished.Project, vanished.Name)
		}
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()

	fmt.Fprintf(out, "DevMan daemon listening on %s (pid %d)\n", listener.Address(), listener.Info.PID)
	bus.Emit(dto.EventDaemonReady, "", "", "daemon ready", map[string]any{
		"port":             listener.Info.Port,
		"graceful_signals": listener.Info.GracefulSignals,
	})

	select {
	case err := <-serveErr:
		if err != nil {
			_ = listener.Release()
			return err
		}
	case <-server.ShutdownRequested():
		// The API already stopped the services before asking for shutdown.
	case sig := <-signals:
		fmt.Fprintf(out, "received %s, stopping services\n", sig)
		sup.StopAll()
	}

	if err := server.GracefulShutdown(); err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	fmt.Fprintln(out, "DevMan daemon stopped")
	return nil
}
