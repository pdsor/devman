// Command httpserver is the container fixture for DevMan's Docker Compose
// integration suite.
//
// It exists so the suite needs no registry. A statically linked Go binary in a
// FROM scratch image builds in a second, runs identically on every engine, and
// cannot fail because Docker Hub is rate limiting, unreachable behind a proxy or
// slow — none of which say anything about DevMan.
//
// Being purpose-built also makes the assertions sharper than a stock image
// would: readiness can be delayed on demand, the exit code and timing are
// controllable, and state written to a volume is reported back on stdout so a
// test can prove a volume survived a stop.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	port := flag.Int("port", envInt("PORT", 80), "port to listen on")
	readyAfter := flag.Duration("ready-after", 0, "delay before /health reports ready")
	exitAfter := flag.Duration("exit-after", 0, "exit on purpose after this delay")
	exitCode := flag.Int("exit-code", 1, "code to exit with when -exit-after fires")
	state := flag.String("state", "", "file to count startups in, for volume tests")
	healthcheck := flag.Bool("healthcheck", false, "probe the local /health endpoint and exit")
	flag.Parse()

	if *healthcheck {
		os.Exit(probe(*port))
	}

	var ready atomic.Bool
	if *readyAfter <= 0 {
		ready.Store(true)
	} else {
		go func() {
			time.Sleep(*readyAfter)
			ready.Store(true)
			fmt.Println("fixture is ready")
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			// A container that is up but not yet ready is the entire reason
			// Compose distinguishes service_started from service_healthy.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("devman fixture"))
	})

	listener, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", *port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot listen:", err)
		os.Exit(1)
	}

	if *state != "" {
		fmt.Printf("state %d\n", bumpState(*state))
	}
	fmt.Printf("fixture listening on %d\n", *port)

	server := &http.Server{Handler: mux}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "serve failed:", err)
		}
	}()

	if *exitAfter > 0 {
		go func() {
			time.Sleep(*exitAfter)
			fmt.Fprintf(os.Stderr, "fixture exiting with %d on purpose\n", *exitCode)
			os.Exit(*exitCode)
		}()
	}

	// SIGTERM is what `docker compose stop` sends. Exiting 0 keeps a deliberate
	// stop from looking like a crash.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	<-signals
	fmt.Println("fixture stopping")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

// probe is the container's own healthcheck command, so the compose fixture needs
// no shell or curl in the image.
func probe(port int) int {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// bumpState increments a counter stored in a file, which is how a test proves a
// named volume outlived a container.
func bumpState(path string) int {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o777)
	}
	current := 0
	if raw, err := os.ReadFile(path); err == nil {
		current, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
	}
	current++
	_ = os.WriteFile(path, []byte(strconv.Itoa(current)), 0o666)
	return current
}

func envInt(name string, fallback int) int {
	if raw := strings.TrimSpace(os.Getenv(name)); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil {
			return value
		}
	}
	return fallback
}
