package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ExecutionFingerprint hashes only the fields that grant DevMan the ability to
// execute something on the user's machine.
//
// Trust is bound to this fingerprint rather than to the raw YAML so that
// harmless edits (display_name, health interval, restart tuning) never force
// the user to re-approve a project, while any change to what actually runs
// does.
//
// Covered fields, per service:
//
//	runtime, cwd, command, args, shell, env_file, env,
//	compose.{file,service,project}, platform.<os>.{command,args,cwd,env}
//
// Adding or removing a service also changes the fingerprint.
func (c *Config) ExecutionFingerprint() string {
	sum := sha256.Sum256([]byte(c.executionCanonicalForm()))
	return hex.EncodeToString(sum[:])
}

// executionCanonicalForm is the stable text that ExecutionFingerprint hashes.
// It is exported through ExplainExecution for auditing and debugging.
func (c *Config) executionCanonicalForm() string {
	var sb strings.Builder
	sb.WriteString("devman-exec-fingerprint/1\n")

	names := make([]string, 0, len(c.Services))
	for name := range c.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		svc := c.Services[name]
		if svc == nil {
			continue
		}
		writeField(&sb, name, "runtime", string(svc.Runtime))
		writeField(&sb, name, "cwd", svc.CWD)
		writeField(&sb, name, "command", svc.Command)
		writeField(&sb, name, "args", strconv.Quote(strings.Join(svc.Args, "\x00")))
		writeField(&sb, name, "shell", fmt.Sprintf("%t/%s", svc.Shell.Enabled, svc.Shell.Type))
		writeField(&sb, name, "env_file", strings.Join(svc.EnvFile, "\x00"))
		for _, key := range sortedKeys(svc.Env) {
			writeField(&sb, name, "env."+key, svc.Env[key])
		}
		if svc.Compose != nil {
			writeField(&sb, name, "compose.file", svc.Compose.File)
			writeField(&sb, name, "compose.service", svc.Compose.Service)
			writeField(&sb, name, "compose.project", svc.Compose.Project)
		}
		for _, platform := range sortedKeys(svc.Platform) {
			overlay := svc.Platform[platform]
			if overlay == nil {
				continue
			}
			prefix := "platform." + platform
			writeField(&sb, name, prefix+".command", overlay.Command)
			writeField(&sb, name, prefix+".args", strconv.Quote(strings.Join(overlay.Args, "\x00")))
			writeField(&sb, name, prefix+".cwd", overlay.CWD)
			for _, key := range sortedKeys(overlay.Env) {
				writeField(&sb, name, prefix+".env."+key, overlay.Env[key])
			}
		}
	}
	return sb.String()
}

func writeField(sb *strings.Builder, service, field, value string) {
	sb.WriteString(service)
	sb.WriteByte('\t')
	sb.WriteString(field)
	sb.WriteByte('\t')
	sb.WriteString(value)
	sb.WriteByte('\n')
}

// ExecutionSummary describes, in user-facing terms, what registering a project
// authorises DevMan to run. The trust prompt renders this.
type ExecutionSummary struct {
	Service     string   `json:"service"`
	Runtime     string   `json:"runtime"`
	CWD         string   `json:"cwd"`
	CommandLine string   `json:"command_line,omitempty"`
	Shell       string   `json:"shell,omitempty"`
	EnvFiles    []string `json:"env_files,omitempty"`
	Compose     string   `json:"compose,omitempty"`
}

// ExplainExecution builds the trust prompt payload for the current platform.
func (c *Config) ExplainExecution(platform string) []ExecutionSummary {
	if platform == "" {
		platform = CurrentPlatform()
	}
	out := make([]ExecutionSummary, 0, len(c.Services))
	for _, name := range c.ServiceNames() {
		svc := c.Services[name]
		exec := svc.Execution(platform)
		summary := ExecutionSummary{
			Service:  name,
			Runtime:  string(svc.Runtime),
			CWD:      exec.CWD,
			EnvFiles: svc.EnvFile,
		}
		if exec.Command != "" {
			parts := append([]string{exec.Command}, exec.Args...)
			summary.CommandLine = strings.Join(parts, " ")
		}
		if svc.Shell.Enabled {
			summary.Shell = "shell"
			if svc.Shell.Type != ShellDefault {
				summary.Shell = string(svc.Shell.Type)
			}
		}
		if svc.Compose != nil {
			file := svc.Compose.File
			if file == "" {
				file = "docker-compose.yml"
			}
			summary.Compose = file + " / " + svc.Compose.Service
		}
		out = append(out, summary)
	}
	return out
}
