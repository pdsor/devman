// Package registry owns project registration, the trust model and the parsed
// configuration cache.
//
// devman.yaml is the only source of truth for service definitions. The database
// stores where a project is, which execution fingerprint the user approved, and
// runtime state. Configuration is re-read whenever the file changes on disk, so
// an edit takes effect without any reload command, but an unchanged file is not
// re-parsed or re-validated on every status call.
package registry

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/devman-project/devman/internal/storage"
	"github.com/devman-project/devman/pkg/config"
	"github.com/devman-project/devman/pkg/errs"
)

// Registry is the project registry.
type Registry struct {
	db *storage.DB

	mu    sync.Mutex
	cache map[string]*cacheEntry
}

type cacheEntry struct {
	config  *config.Config
	path    string
	modTime time.Time
	size    int64
	// validation is the result recorded when the file was last parsed.
	validation *config.ValidationResult
}

// New creates a registry over a database.
func New(db *storage.DB) *Registry {
	return &Registry{db: db, cache: map[string]*cacheEntry{}}
}

// Preview is what the user (or an agent's user) must approve before a project
// is registered: exactly which commands DevMan will be allowed to run.
type Preview struct {
	ID          string                    `json:"id"`
	Name        string                    `json:"name"`
	Path        string                    `json:"path"`
	ConfigPath  string                    `json:"config_path"`
	Fingerprint string                    `json:"execution_fingerprint"`
	Execution   []config.ExecutionSummary `json:"execution"`
	Validation  *config.ValidationResult  `json:"validation"`
	// AlreadyRegistered reports that this path is known; registering again
	// updates it.
	AlreadyRegistered bool `json:"already_registered"`
	// TrustRequired reports that starting this project needs approval, either
	// because it is new or because its execution fingerprint changed.
	TrustRequired bool `json:"trust_required"`
}

// Inspect loads and validates a project directory without registering it.
func (r *Registry) Inspect(path string) (*Preview, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInvalidRequest, err, "cannot resolve %q", path)
	}
	cfg, err := config.Load(abs)
	if err != nil {
		return nil, err
	}
	validation := cfg.Validate(config.DefaultValidateOptions())
	fingerprint := cfg.ExecutionFingerprint()

	preview := &Preview{
		ID:          storage.ProjectID(cfg.ProjectRoot),
		Name:        cfg.Project.Name,
		Path:        cfg.ProjectRoot,
		ConfigPath:  cfg.ConfigPath,
		Fingerprint: fingerprint,
		Execution:   cfg.ExplainExecution(config.CurrentPlatform()),
		Validation:  validation,
	}
	if existing, err := r.db.Project(preview.ID); err == nil {
		preview.AlreadyRegistered = true
		preview.TrustRequired = existing.TrustedFingerprint != fingerprint
	} else {
		preview.TrustRequired = true
	}
	return preview, nil
}

// Register adds (or updates) a project.
//
// trust must be set explicitly. Interactive callers show Inspect's execution
// summary and pass the user's answer; non-interactive callers such as the Codex
// skill must pass --trust, which the skill may only do after the user has
// approved the commands it found.
func (r *Registry) Register(path string, trust bool) (storage.ProjectRecord, error) {
	preview, err := r.Inspect(path)
	if err != nil {
		return storage.ProjectRecord{}, err
	}
	if !preview.Validation.Valid {
		return storage.ProjectRecord{}, preview.Validation.Err()
	}

	record := storage.ProjectRecord{
		ID:         preview.ID,
		Name:       preview.Name,
		Path:       preview.Path,
		ConfigPath: preview.ConfigPath,
	}
	if existing, err := r.db.Project(preview.ID); err == nil {
		record.CreatedAt = existing.CreatedAt
		record.Favorite = existing.Favorite
		record.LastStartedAt = existing.LastStartedAt
		record.TrustedFingerprint = existing.TrustedFingerprint
	}
	if trust {
		record.TrustedFingerprint = preview.Fingerprint
	}
	if err := r.db.UpsertProject(record); err != nil {
		return storage.ProjectRecord{}, err
	}
	r.invalidate(record.ID)
	return r.db.Project(record.ID)
}

// Unregister removes a project from the registry. Running services must be
// stopped by the caller first; the registry itself has no process control.
func (r *Registry) Unregister(id string) error {
	if err := r.db.DeleteProject(id); err != nil {
		return err
	}
	r.invalidate(id)
	return nil
}

// Trust approves the project's current execution fingerprint.
func (r *Registry) Trust(id string) error {
	cfg, err := r.Config(id)
	if err != nil {
		return err
	}
	return r.db.SetProjectTrust(id, cfg.ExecutionFingerprint())
}

// Revoke withdraws trust.
func (r *Registry) Revoke(id string) error { return r.db.SetProjectTrust(id, "") }

// EnsureTrusted fails with PROJECT_UNTRUSTED when the configuration's execution
// fingerprint does not match the approved one.
//
// Only execution-relevant fields feed the fingerprint, so editing a display
// name or a health interval never triggers this, while changing a command,
// args, cwd, shell, env, env_file, runtime or compose target always does.
func (r *Registry) EnsureTrusted(id string) error {
	project, err := r.db.Project(id)
	if err != nil {
		return err
	}
	cfg, err := r.Config(id)
	if err != nil {
		return err
	}
	current := cfg.ExecutionFingerprint()
	if project.TrustedFingerprint == "" {
		return errs.New(errs.CodeProjectUntrusted,
			"project %q has not been trusted; run `devman register --trust` after reviewing its commands",
			project.Name).With("project", project.Name)
	}
	if project.TrustedFingerprint != current {
		return errs.New(errs.CodeProjectUntrusted,
			"the commands declared by %q changed since it was trusted; review and trust it again",
			project.Name).
			With("project", project.Name).
			With("trusted_fingerprint", project.TrustedFingerprint).
			With("current_fingerprint", current)
	}
	return nil
}

// SetFavorite toggles the favourite flag.
func (r *Registry) SetFavorite(id string, favorite bool) error {
	return r.db.SetProjectFavorite(id, favorite)
}

// Projects lists registered projects.
func (r *Registry) Projects() ([]storage.ProjectRecord, error) { return r.db.Projects() }

// Project returns one project by id.
func (r *Registry) Project(id string) (storage.ProjectRecord, error) { return r.db.Project(id) }

// Resolve maps a CLI selector onto a project.
//
// An empty selector means "the project containing the working directory",
// walking up from cwd so `cd my-project/frontend && devman start` works.
// Service names are never searched across projects: an ambiguous `backend`
// would be a trap, so an unresolvable selector is a hard PROJECT_NOT_FOUND.
func (r *Registry) Resolve(selector, cwd string) (storage.ProjectRecord, error) {
	if selector == "" {
		return r.projectContaining(cwd)
	}
	if project, err := r.db.Project(selector); err == nil {
		return project, nil
	}
	if project, err := r.db.ProjectByName(selector); err == nil {
		return project, nil
	}
	// A path, absolute or relative to cwd.
	candidate := selector
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(cwd, candidate)
	}
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		if project, err := r.db.ProjectByPath(candidate); err == nil {
			return project, nil
		}
	}
	return storage.ProjectRecord{}, errs.New(errs.CodeProjectNotFound,
		"no registered project matches %q", selector)
}

func (r *Registry) projectContaining(cwd string) (storage.ProjectRecord, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return storage.ProjectRecord{}, errs.Wrap(errs.CodeInvalidRequest, err,
			"cannot resolve working directory")
	}
	dir := abs
	for {
		if project, err := r.db.ProjectByPath(dir); err == nil {
			return project, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return storage.ProjectRecord{}, errs.New(errs.CodeProjectNotFound,
		"%s is not inside a registered project; pass a project name or run `devman register .`", abs)
}

// Config returns the parsed configuration of a project, re-reading it only when
// the file changed on disk.
func (r *Registry) Config(id string) (*config.Config, error) {
	cfg, _, err := r.ConfigWithValidation(id)
	return cfg, err
}

// ConfigWithValidation returns the configuration plus the validation result
// recorded when it was last parsed.
func (r *Registry) ConfigWithValidation(id string) (*config.Config, *config.ValidationResult, error) {
	project, err := r.db.Project(id)
	if err != nil {
		return nil, nil, err
	}

	info, statErr := os.Stat(project.ConfigPath)
	if statErr != nil {
		// The config may have moved within the project; rediscover it.
		discovered, discoverErr := config.Discover(project.Path)
		if discoverErr != nil {
			return nil, nil, errs.New(errs.CodeConfigNotFound,
				"configuration for %q is missing: %s", project.Name, project.ConfigPath)
		}
		project.ConfigPath = discovered
		info, statErr = os.Stat(discovered)
		if statErr != nil {
			return nil, nil, errs.Wrap(errs.CodeConfigNotFound, statErr, "cannot read configuration")
		}
	}

	r.mu.Lock()
	entry, cached := r.cache[id]
	if cached && entry.path == project.ConfigPath &&
		entry.modTime.Equal(info.ModTime()) && entry.size == info.Size() {
		defer r.mu.Unlock()
		return entry.config, entry.validation, nil
	}
	r.mu.Unlock()

	cfg, err := config.Load(project.ConfigPath)
	if err != nil {
		return nil, nil, err
	}
	validation := cfg.Validate(config.DefaultValidateOptions())

	r.mu.Lock()
	r.cache[id] = &cacheEntry{
		config:     cfg,
		path:       project.ConfigPath,
		modTime:    info.ModTime(),
		size:       info.Size(),
		validation: validation,
	}
	r.mu.Unlock()
	return cfg, validation, nil
}

// ValidConfig returns the configuration only when it currently validates.
func (r *Registry) ValidConfig(id string) (*config.Config, error) {
	cfg, validation, err := r.ConfigWithValidation(id)
	if err != nil {
		return nil, err
	}
	if !validation.Valid {
		return nil, validation.Err()
	}
	return cfg, nil
}

func (r *Registry) invalidate(id string) {
	r.mu.Lock()
	delete(r.cache, id)
	r.mu.Unlock()
}

// ProjectDisplayName prefers the configured display name.
func ProjectDisplayName(record storage.ProjectRecord, cfg *config.Config) string {
	if cfg != nil && strings.TrimSpace(cfg.Project.DisplayName) != "" {
		return cfg.Project.DisplayName
	}
	return record.Name
}
