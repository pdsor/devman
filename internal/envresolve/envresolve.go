// Package envresolve builds the environment a service runs with, and resolves
// executables in the face of a reduced PATH.
//
// Precedence, lowest to highest:
//
//	daemon inherited environment
//	env_file entries, in declaration order
//	service env
//	platform.<os>.env
//	DevMan runtime injection (ports, DEVMAN_*)
//
// Runtime injection wins on purpose: declaring `ports: [{value: auto, env: PORT}]`
// is an explicit statement that DevMan owns that variable, so a stale PORT in
// .env must not override the allocated one. A project that does not want this
// simply omits `env:` from the port.
//
// ${ENV:NAME} resolves against the user layers only, never against injection,
// so a template can never depend on a value that has not been allocated yet.
package envresolve

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/devman-project/devman/pkg/errs"
)

// Layers holds the environment sources of one service.
type Layers struct {
	// Base is the daemon's own environment.
	Base map[string]string
	// Files are parsed env_file contents, in declaration order.
	Files []map[string]string
	// Service is the service env with the platform overlay already applied.
	Service map[string]string
	// Injection is DevMan's own variables (allocated ports, DEVMAN_*).
	Injection map[string]string
}

// UserEnv merges everything except DevMan injection. This is the layer
// ${ENV:NAME} sees and the layer required_env is validated against.
func (l Layers) UserEnv() map[string]string {
	out := make(map[string]string, len(l.Base)+len(l.Service)+8)
	for key, value := range l.Base {
		out[key] = value
	}
	for _, file := range l.Files {
		for key, value := range file {
			out[key] = value
		}
	}
	for key, value := range l.Service {
		out[key] = value
	}
	return out
}

// Final merges every layer, with injection last.
func (l Layers) Final() map[string]string {
	out := l.UserEnv()
	for key, value := range l.Injection {
		out[key] = value
	}
	return out
}

// Environ renders a merged map as a sorted KEY=VALUE slice, which keeps process
// creation deterministic and diffable in logs.
func Environ(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}

// CurrentEnv snapshots the daemon environment.
func CurrentEnv() map[string]string {
	out := map[string]string{}
	for _, entry := range os.Environ() {
		if key, value, found := strings.Cut(entry, "="); found {
			out[key] = value
		}
	}
	return out
}

// LoadFile parses one env file. A missing file is not an error: `.env.local`
// being absent is normal, and validation already warns about it.
func LoadFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, errs.Wrap(errs.CodeInternal, err, "cannot read %s", path)
	}
	defer file.Close()

	out := map[string]string{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		out[key] = unquote(strings.TrimSpace(value))
	}
	if err := scanner.Err(); err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "cannot read %s", path)
	}
	return out, nil
}

// LoadFiles parses several env files relative to a base directory.
func LoadFiles(baseDir string, paths []string) ([]map[string]string, error) {
	out := make([]map[string]string, 0, len(paths))
	for _, path := range paths {
		full := path
		if !filepath.IsAbs(full) {
			full = filepath.Join(baseDir, path)
		}
		parsed, err := LoadFile(full)
		if err != nil {
			return nil, err
		}
		out = append(out, parsed)
	}
	return out, nil
}

// unquote strips matching quotes and, for double-quoted values, unescapes the
// common sequences dotenv writers produce.
func unquote(value string) string {
	if len(value) < 2 {
		return value
	}
	first, last := value[0], value[len(value)-1]
	if first != last {
		// An unquoted value may carry a trailing inline comment.
		if idx := strings.Index(value, " #"); idx >= 0 {
			return strings.TrimSpace(value[:idx])
		}
		return value
	}
	switch first {
	case '\'':
		return value[1 : len(value)-1]
	case '"':
		inner := value[1 : len(value)-1]
		inner = strings.ReplaceAll(inner, `\n`, "\n")
		inner = strings.ReplaceAll(inner, `\r`, "\r")
		inner = strings.ReplaceAll(inner, `\t`, "\t")
		inner = strings.ReplaceAll(inner, `\"`, `"`)
		inner = strings.ReplaceAll(inner, `\\`, `\`)
		return inner
	default:
		return value
	}
}

// MissingRequired returns the declared required_env names that are absent or
// empty in the user layer.
func MissingRequired(required []string, env map[string]string) []string {
	var missing []string
	for _, name := range required {
		if value, ok := env[name]; !ok || strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

// ToolNames are the executables DevMan probes so the GUI can show what it can
// actually reach. A GUI launched from a desktop menu often has a much smaller
// PATH than an interactive shell.
var ToolNames = []string{
	"node", "npm", "pnpm", "yarn", "bun",
	"python", "python3", "uv", "poetry", "pipenv",
	"go", "cargo", "rustc",
	"java", "php", "ruby",
	"docker", "docker-compose", "git",
}

// Resolver resolves executables using the daemon PATH plus any extra
// directories configured in the global settings.
type Resolver struct {
	// AdditionalPath entries are prepended to PATH.
	AdditionalPath []string
	// ExtraEnv is applied to every service (global settings).
	ExtraEnv map[string]string
}

// PathValue returns the PATH the daemon should use for lookups and for child
// processes.
func (r Resolver) PathValue(base map[string]string) string {
	current := base[pathKey(base)]
	if len(r.AdditionalPath) == 0 {
		return current
	}
	parts := append([]string(nil), r.AdditionalPath...)
	if current != "" {
		parts = append(parts, current)
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

// pathKey finds the PATH variable, which is case-insensitive on Windows.
func pathKey(env map[string]string) string {
	if _, ok := env["PATH"]; ok {
		return "PATH"
	}
	if runtime.GOOS == "windows" {
		for key := range env {
			if strings.EqualFold(key, "PATH") {
				return key
			}
		}
	}
	return "PATH"
}

// ApplyPath rewrites the PATH entry of an environment map in place.
func (r Resolver) ApplyPath(env map[string]string) {
	key := pathKey(env)
	value := r.PathValue(env)
	if value != "" {
		env[key] = value
	}
	for name, extra := range r.ExtraEnv {
		if _, exists := env[name]; !exists {
			env[name] = extra
		}
	}
}

// Lookup resolves an executable name against a specific PATH and working
// directory.
//
// The resolved absolute path is runtime state; it is never written back into
// devman.yaml, which has to stay portable across machines and platforms.
func (r Resolver) Lookup(command, cwd, pathValue string) (string, error) {
	if command == "" {
		return "", errs.New(errs.CodeCommandNotFound, "no command specified")
	}
	// An explicit relative or absolute path is resolved against the cwd.
	if strings.ContainsAny(command, `/\`) {
		candidate := command
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(cwd, candidate)
		}
		if isExecutableFile(candidate) {
			return candidate, nil
		}
		if runtime.GOOS == "windows" {
			if resolved, ok := withWindowsExtensions(candidate); ok {
				return resolved, nil
			}
		}
		return "", errs.New(errs.CodeCommandNotFound, "%s is not an executable file", candidate)
	}

	restore, err := withPath(pathValue)
	if err == nil {
		defer restore()
	}
	resolved, lookErr := exec.LookPath(command)
	if lookErr != nil {
		return "", errs.New(errs.CodeCommandNotFound,
			"command %q was not found on PATH", command).With("command", command)
	}
	if !filepath.IsAbs(resolved) {
		if abs, absErr := filepath.Abs(resolved); absErr == nil {
			resolved = abs
		}
	}
	return resolved, nil
}

// withPath temporarily sets the process PATH so exec.LookPath honours the
// daemon's configured additional directories.
func withPath(pathValue string) (func(), error) {
	if pathValue == "" {
		return func() {}, nil
	}
	previous, had := os.LookupEnv("PATH")
	if err := os.Setenv("PATH", pathValue); err != nil {
		return func() {}, err
	}
	return func() {
		if had {
			_ = os.Setenv("PATH", previous)
		} else {
			_ = os.Unsetenv("PATH")
		}
	}, nil
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
}

func withWindowsExtensions(path string) (string, bool) {
	extensions := strings.Split(os.Getenv("PATHEXT"), string(os.PathListSeparator))
	if len(extensions) == 0 || extensions[0] == "" {
		extensions = []string{".COM", ".EXE", ".BAT", ".CMD"}
	}
	for _, extension := range extensions {
		candidate := path + strings.ToLower(extension)
		if isExecutableFile(candidate) {
			return candidate, true
		}
	}
	return "", false
}

// ProbeTools resolves the well-known development tools.
func (r Resolver) ProbeTools(base map[string]string) map[string]string {
	pathValue := r.PathValue(base)
	restore, err := withPath(pathValue)
	if err == nil {
		defer restore()
	}
	out := map[string]string{}
	for _, name := range ToolNames {
		if resolved, lookErr := exec.LookPath(name); lookErr == nil {
			out[name] = resolved
		}
	}
	return out
}
