// Package errs defines the unified DevMan error model shared by the CLI,
// the HTTP API and the Codex skill.
package errs

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is a stable, machine readable error identifier. Never rename a code
// once released: the Codex skill and the GUI branch on these values.
type Code string

const (
	CodeConfigInvalid    Code = "CONFIG_INVALID"
	CodeConfigNotFound   Code = "CONFIG_NOT_FOUND"
	CodeProjectNotFound  Code = "PROJECT_NOT_FOUND"
	CodeProjectExists    Code = "PROJECT_EXISTS"
	CodeProjectUntrusted Code = "PROJECT_UNTRUSTED"
	CodeServiceNotFound  Code = "SERVICE_NOT_FOUND"
	CodePortConflict     Code = "PORT_CONFLICT"
	CodePortExhausted    Code = "PORT_EXHAUSTED"
	CodeCommandNotFound  Code = "COMMAND_NOT_FOUND"
	CodeEnvMissing       Code = "ENV_MISSING"
	CodeDependencyFailed Code = "DEPENDENCY_FAILED"
	CodeHealthcheckFail  Code = "HEALTHCHECK_FAILED"
	CodeProcessCrashed   Code = "PROCESS_CRASHED"
	CodeDockerNotFound   Code = "DOCKER_NOT_FOUND"
	CodeServiceBlocked   Code = "SERVICE_BLOCKED"
	CodeAlreadyRunning   Code = "ALREADY_RUNNING"
	CodeNotRunning       Code = "NOT_RUNNING"
	CodeDaemonNotRunning Code = "DAEMON_NOT_RUNNING"
	CodeUnauthorized     Code = "UNAUTHORIZED"
	CodeInternal         Code = "INTERNAL"
	CodeUnsupported      Code = "UNSUPPORTED"
	CodeInvalidRequest   Code = "INVALID_REQUEST"
	CodeTimeout          Code = "TIMEOUT"
)

// Error is the canonical DevMan error. It serialises directly into API
// responses and `--json` CLI output.
type Error struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Path    string         `json:"path,omitempty"`
	Details map[string]any `json:"details,omitempty"`

	wrapped error
}

func (e *Error) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Path)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.wrapped }

// New builds an error with a code and a formatted message.
func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap attaches a code to an existing error.
func Wrap(code Code, err error, format string, args ...any) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		Code:    code,
		Message: fmt.Sprintf(format, args...) + ": " + err.Error(),
		wrapped: err,
	}
}

// At returns a copy of the error annotated with a config path such as
// "services.backend.cwd".
func (e *Error) At(path string) *Error {
	clone := *e
	clone.Path = path
	return &clone
}

// With attaches a detail key/value pair.
func (e *Error) With(key string, value any) *Error {
	clone := *e
	clone.Details = make(map[string]any, len(e.Details)+1)
	for k, v := range e.Details {
		clone.Details[k] = v
	}
	clone.Details[key] = value
	return &clone
}

// From normalises any error into a *Error.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var de *Error
	if errors.As(err, &de) {
		return de
	}
	return &Error{Code: CodeInternal, Message: err.Error(), wrapped: err}
}

// CodeOf reports the code of err, or CodeInternal.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	return From(err).Code
}

// Is reports whether err carries the given code.
func Is(err error, code Code) bool {
	if err == nil {
		return false
	}
	return From(err).Code == code
}

// HTTPStatus maps a code onto an HTTP status for the local API.
func (e *Error) HTTPStatus() int {
	switch e.Code {
	case CodeProjectNotFound, CodeServiceNotFound, CodeConfigNotFound:
		return http.StatusNotFound
	case CodeProjectExists, CodeAlreadyRunning, CodeNotRunning, CodePortConflict:
		return http.StatusConflict
	case CodeUnauthorized:
		// A missing or wrong credential, including a browser origin the daemon
		// does not accept.
		return http.StatusUnauthorized
	case CodeProjectUntrusted:
		// Authenticated, but the project's execution fingerprint has not been
		// approved: the request is understood and deliberately refused.
		return http.StatusForbidden
	case CodeConfigInvalid, CodeInvalidRequest, CodeEnvMissing, CodeServiceBlocked:
		return http.StatusBadRequest
	case CodeCommandNotFound, CodeDockerNotFound, CodePortExhausted, CodeUnsupported:
		return http.StatusUnprocessableEntity
	case CodeTimeout:
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}
