// Command build is DevMan's own release tooling.
//
// It is written in Go rather than as a Makefile or a shell script on purpose.
// DevMan ships a single CGO-free binary for three operating systems, and the
// tool that produces it should not require anything the project does not
// already require: `go run ./tools/build dist` behaves identically on Windows,
// macOS and Linux, so a release built on a laptop and a release built in CI go
// through the same code.
//
// Targets:
//
//	version   print the version that would be stamped into a build
//	dist      cross-compile the CLI for every supported platform, archive it
//	          and write SHA256SUMS
//	sidecar   build the CLI for the host and place it where the Tauri bundler
//	          picks it up, so the installed desktop app ships its own daemon
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// platform is one release target. Cross-compilation is free because the SQLite
// driver is pure Go; the moment that stops being true this list has to shrink
// to whatever CI can natively build.
type platform struct {
	goos   string
	goarch string
}

var platforms = []platform{
	{"windows", "amd64"},
	{"windows", "arm64"},
	{"darwin", "amd64"},
	{"darwin", "arm64"},
	{"linux", "amd64"},
	{"linux", "arm64"},
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	root, err := repoRoot()
	if err != nil {
		fail(err)
	}

	switch os.Args[1] {
	case "version":
		fmt.Println(version(root))
	case "dist":
		if err := dist(root); err != nil {
			fail(err)
		}
	case "sidecar":
		if err := sidecar(root); err != nil {
			fail(err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: go run ./tools/build <version|dist|sidecar>")
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "build: %v\n", err)
	os.Exit(1)
}

// repoRoot walks up from the working directory looking for go.mod, so the tool
// works whether it is invoked from the repository root or from a subdirectory.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found above the working directory")
		}
		dir = parent
	}
}

// version resolves what to stamp into the binary.
//
// DEVMAN_VERSION wins, because a tagged CI release knows its own name. Failing
// that the tool asks git, and the -dirty suffix is kept deliberately: a binary
// built from uncommitted work should say so rather than impersonate a tag.
func version(root string) string {
	if explicit := strings.TrimSpace(os.Getenv("DEVMAN_VERSION")); explicit != "" {
		return strings.TrimPrefix(explicit, "v")
	}
	out, err := run(root, nil, "git", "describe", "--tags", "--always", "--dirty")
	if err != nil {
		return "0.1.0-dev"
	}
	return strings.TrimPrefix(strings.TrimSpace(out), "v")
}

func binaryName(goos string) string {
	if goos == "windows" {
		return "devman.exe"
	}
	return "devman"
}

// buildCLI compiles cmd/devman for one platform.
//
// -trimpath keeps absolute build paths out of the binary so two machines
// produce the same bytes; the version is injected through the variable
// cmd/devman/main.go documents.
func buildCLI(root, goos, goarch, output, version string) error {
	env := []string{
		"GOOS=" + goos,
		"GOARCH=" + goarch,
		"CGO_ENABLED=0",
	}
	ldflags := fmt.Sprintf("-s -w -X main.Version=%s", version)
	_, err := run(root, env, "go", "build", "-trimpath", "-ldflags", ldflags, "-o", output, "./cmd/devman")
	return err
}

func dist(root string) error {
	v := version(root)
	out := filepath.Join(root, "dist")
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}

	fmt.Printf("version %s\n", v)

	archives := make([]string, 0, len(platforms))
	for _, p := range platforms {
		stage := filepath.Join(out, fmt.Sprintf("devman_%s_%s_%s", v, p.goos, p.goarch))
		if err := os.MkdirAll(stage, 0o755); err != nil {
			return err
		}
		binary := filepath.Join(stage, binaryName(p.goos))
		fmt.Printf("building %s/%s\n", p.goos, p.goarch)
		if err := buildCLI(root, p.goos, p.goarch, binary, v); err != nil {
			return fmt.Errorf("%s/%s: %w", p.goos, p.goarch, err)
		}
		if err := copyFile(filepath.Join(root, "README.md"), filepath.Join(stage, "README.md")); err != nil {
			return err
		}

		var archive string
		if p.goos == "windows" {
			archive = stage + ".zip"
			err := writeZip(archive, stage)
			if err != nil {
				return err
			}
		} else {
			archive = stage + ".tar.gz"
			if err := writeTarGz(archive, stage); err != nil {
				return err
			}
		}
		if err := os.RemoveAll(stage); err != nil {
			return err
		}
		archives = append(archives, archive)
		fmt.Printf("packaged %s\n", filepath.Base(archive))
	}

	if err := writeChecksums(filepath.Join(out, "SHA256SUMS"), archives); err != nil {
		return err
	}
	fmt.Println("wrote dist/SHA256SUMS")
	return nil
}

// sidecar builds the CLI for the host and drops it where tauri.conf.json's
// externalBin entry expects it.
//
// Tauri identifies a sidecar by Rust target triple and strips the triple when
// bundling, which lands the binary next to the application executable — the
// exact path the desktop shell's devman_executable() looks at before falling
// back to PATH. That fallback is not enough on its own: a GUI started from the
// Start menu or the Dock often has a PATH the user's shell would not recognise.
func sidecar(root string) error {
	triple, err := hostTriple(root)
	if err != nil {
		return err
	}
	v := version(root)
	dir := filepath.Join(root, "apps", "desktop", "src-tauri", "binaries")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	name := "devman-" + triple
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	output := filepath.Join(dir, name)
	if err := buildCLI(root, runtime.GOOS, runtime.GOARCH, output, v); err != nil {
		return err
	}
	fmt.Printf("sidecar %s (version %s)\n", filepath.Join("apps", "desktop", "src-tauri", "binaries", name), v)
	return nil
}

// hostTriple asks rustc for the host target triple rather than deriving it from
// GOOS/GOARCH. A hand-written mapping would be a second source of truth that
// only fails at bundle time, and rustc is already required to build the shell.
func hostTriple(root string) (string, error) {
	out, err := run(root, nil, "rustc", "-vV")
	if err != nil {
		return "", fmt.Errorf("rustc is required to name the sidecar: %w", err)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "host: "); ok {
			return strings.TrimSpace(after), nil
		}
	}
	return "", fmt.Errorf("rustc -vV did not report a host triple")
}

func run(dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		return "", fmt.Errorf("%s: %w: %s", name, err, detail)
	}
	return string(out), nil
}

func copyFile(from, to string) error {
	source, err := os.Open(from)
	if err != nil {
		return err
	}
	defer source.Close()

	target, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(target, source); err != nil {
		target.Close()
		return err
	}
	return target.Close()
}

func writeZip(archive, stage string) error {
	file, err := os.Create(archive)
	if err != nil {
		return err
	}
	writer := zip.NewWriter(file)
	err = walkStage(stage, func(path, name string, info fs.FileInfo) error {
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = name
		header.Method = zip.Deflate
		entry, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		return copyInto(entry, path)
	})
	if err != nil {
		writer.Close()
		file.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func writeTarGz(archive, stage string) error {
	file, err := os.Create(archive)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(file)
	writer := tar.NewWriter(gz)
	err = walkStage(stage, func(path, name string, info fs.FileInfo) error {
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = name
		// The archive is produced on whatever machine cut the release, so the
		// executable bit is set here rather than inherited from the filesystem
		// that built it — Windows would otherwise ship a non-executable binary
		// to macOS and Linux.
		if strings.HasSuffix(name, "devman") {
			header.Mode = 0o755
		} else {
			header.Mode = 0o644
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		return copyInto(writer, path)
	})
	if err != nil {
		writer.Close()
		gz.Close()
		file.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		gz.Close()
		file.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func walkStage(stage string, visit func(path, name string, info fs.FileInfo) error) error {
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if err := visit(filepath.Join(stage, entry.Name()), entry.Name(), info); err != nil {
			return err
		}
	}
	return nil
}

func copyInto(writer io.Writer, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(writer, file)
	return err
}

// writeChecksums emits the usual `sha256sum -c` compatible listing so a
// download can be verified without trusting the page it came from.
func writeChecksums(path string, archives []string) error {
	sort.Strings(archives)
	var listing strings.Builder
	for _, archive := range archives {
		sum, err := sha256File(archive)
		if err != nil {
			return err
		}
		fmt.Fprintf(&listing, "%s  %s\n", sum, filepath.Base(archive))
	}
	return os.WriteFile(path, []byte(listing.String()), 0o644)
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
