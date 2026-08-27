package config

import (
	"strconv"
	"strings"

	"github.com/devman-project/devman/pkg/errs"
)

// Template variable names understood by V0.1.
const (
	VarPort       = "PORT"
	VarProjectDir = "PROJECT_DIR"
	VarServiceDir = "SERVICE_DIR"
	VarHome       = "HOME"
	VarEnv        = "ENV"
)

// TemplateContext supplies values for ${...} expansion.
//
// Env intentionally exposes only the user environment layers (daemon env,
// env_file, service env, platform env). DevMan runtime injection such as the
// allocated PORT is NOT visible through ${ENV:...}: ports are referenced with
// ${PORT} / ${PORT:name} so a template can never accidentally depend on a
// value that has not been allocated yet.
type TemplateContext struct {
	ProjectDir string
	ServiceDir string
	Home       string

	// Ports maps a declared port name to its allocated number.
	Ports map[string]int
	// DefaultPortName resolves a bare ${PORT}.
	DefaultPortName string

	// Env resolves ${ENV:NAME} from the merged user environment.
	Env func(name string) (string, bool)
}

// Lookup resolves a single variable reference such as "PORT" or "ENV:HOME".
func (c TemplateContext) Lookup(ref string) (string, error) {
	name, arg, hasArg := strings.Cut(ref, ":")
	name = strings.TrimSpace(name)
	arg = strings.TrimSpace(arg)

	switch name {
	case VarPort:
		portName := arg
		if !hasArg || portName == "" {
			portName = c.DefaultPortName
		}
		if portName == "" {
			return "", errs.New(errs.CodeConfigInvalid,
				"${PORT} used but the service declares no ports")
		}
		port, ok := c.Ports[portName]
		if !ok {
			return "", errs.New(errs.CodeConfigInvalid,
				"${PORT:%s} refers to an undeclared port name", portName)
		}
		return strconv.Itoa(port), nil
	case VarProjectDir:
		return c.ProjectDir, nil
	case VarServiceDir:
		return c.ServiceDir, nil
	case VarHome:
		return c.Home, nil
	case VarEnv:
		if arg == "" {
			return "", errs.New(errs.CodeConfigInvalid, "${ENV:NAME} requires a variable name")
		}
		if c.Env == nil {
			return "", errs.New(errs.CodeEnvMissing, "environment variable %s is not set", arg)
		}
		value, ok := c.Env(arg)
		if !ok {
			return "", errs.New(errs.CodeEnvMissing, "environment variable %s is not set", arg).
				With("variable", arg)
		}
		return value, nil
	default:
		return "", errs.New(errs.CodeConfigInvalid, "unknown template variable ${%s}", ref)
	}
}

// Expand resolves every ${...} reference in s. `$${` is an escape for a
// literal `${`.
func (c TemplateContext) Expand(s string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			out.WriteByte(s[i])
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			// `$$` escapes a literal `$`.
			out.WriteByte('$')
			i += 2
			continue
		}
		if i+1 >= len(s) || s[i+1] != '{' {
			out.WriteByte(s[i])
			i++
			continue
		}
		end := strings.IndexByte(s[i+2:], '}')
		if end < 0 {
			return "", errs.New(errs.CodeConfigInvalid, "unterminated template expression in %q", s)
		}
		ref := s[i+2 : i+2+end]
		value, err := c.Lookup(ref)
		if err != nil {
			return "", err
		}
		out.WriteString(value)
		i += 2 + end + 1
	}
	return out.String(), nil
}

// ExpandAll expands every element of a slice.
func (c TemplateContext) ExpandAll(values []string) ([]string, error) {
	out := make([]string, len(values))
	for i, v := range values {
		expanded, err := c.Expand(v)
		if err != nil {
			return nil, err
		}
		out[i] = expanded
	}
	return out, nil
}

// TemplateRefs extracts every ${...} reference from s without resolving it.
func TemplateRefs(s string) []string {
	var refs []string
	for i := 0; i < len(s); {
		if s[i] != '$' {
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			i += 2
			continue
		}
		if i+1 >= len(s) || s[i+1] != '{' {
			i++
			continue
		}
		end := strings.IndexByte(s[i+2:], '}')
		if end < 0 {
			return refs
		}
		refs = append(refs, s[i+2:i+2+end])
		i += 2 + end + 1
	}
	return refs
}

// CheckTemplateSyntax validates references without needing runtime values.
// It is used by `devman validate`: variable names must be known and
// ${PORT:name} must refer to a port the service declares.
func CheckTemplateSyntax(s string, declaredPorts []string, hasDefaultPort bool) error {
	if strings.Count(s, "${") > 0 && !strings.Contains(s, "}") {
		return errs.New(errs.CodeConfigInvalid, "unterminated template expression in %q", s)
	}
	for _, ref := range TemplateRefs(s) {
		name, arg, hasArg := strings.Cut(ref, ":")
		name = strings.TrimSpace(name)
		arg = strings.TrimSpace(arg)
		switch name {
		case VarPort:
			if !hasArg || arg == "" {
				if !hasDefaultPort {
					return errs.New(errs.CodeConfigInvalid,
						"${PORT} used but the service declares no ports")
				}
				continue
			}
			if !contains(declaredPorts, arg) {
				return errs.New(errs.CodeConfigInvalid,
					"${PORT:%s} refers to an undeclared port name", arg)
			}
		case VarProjectDir, VarServiceDir, VarHome:
			if hasArg && arg != "" {
				return errs.New(errs.CodeConfigInvalid, "${%s} does not take an argument", name)
			}
		case VarEnv:
			if arg == "" {
				return errs.New(errs.CodeConfigInvalid, "${ENV:NAME} requires a variable name")
			}
			if arg == VarPort {
				return errs.New(errs.CodeConfigInvalid,
					"use ${PORT} instead of ${ENV:PORT}: DevMan allocated ports are not visible to ${ENV:...}")
			}
		default:
			return errs.New(errs.CodeConfigInvalid, "unknown template variable ${%s}", ref)
		}
	}
	return nil
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
