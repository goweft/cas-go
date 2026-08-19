// Package runner executes code workspace content in isolated subprocesses.
// Language is detected from content. Execution is time-bounded.
//
// Isolation is process-level: execution timeout, process-group kill,
// stripped environment, and a temp working directory. It is NOT an
// OS-level sandbox — executed code runs with the invoking user's file
// and network access. seccomp/namespace sandboxing is tracked as future
// hardening; see ARCHITECTURE.md (Code execution).
package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultTimeout is the maximum runtime for a single execution.
const DefaultTimeout = 30 * time.Second

// Result captures the outcome of a code execution.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
	Language string
}

// Lang identifies a programming language for execution.
type Lang struct {
	Name    string   // "python", "go", "bash", "javascript", "ruby"
	Cmd     []string // command to execute: {"python3", tmpfile}
	Ext     string   // temp file extension: ".py"
	Compile []string // optional compile step (Go): {"go", "run", tmpfile}
}

// DetectLang infers the language from code content.
// Returns nil if the language cannot be determined or is unsupported.
func DetectLang(content string) *Lang {
	c := strings.ToLower(content)
	trimmed := strings.TrimSpace(content)

	switch {
	case strings.HasPrefix(trimmed, "#!/bin/bash") ||
		strings.HasPrefix(trimmed, "#!/usr/bin/env bash") ||
		strings.HasPrefix(trimmed, "#!/bin/sh"):
		return &Lang{Name: "bash", Ext: ".sh", Cmd: []string{"bash"}}

	case strings.Contains(c, "package main") ||
		strings.Contains(c, "func main()") ||
		(strings.Contains(c, "import (") && strings.Contains(c, "fmt")):
		return &Lang{Name: "go", Ext: ".go", Compile: []string{"go", "run"}}

	case strings.Contains(c, "def ") && strings.Contains(c, ":") ||
		strings.Contains(c, "import ") && strings.Contains(c, "print(") ||
		strings.HasPrefix(trimmed, "#!/usr/bin/env python") ||
		strings.HasPrefix(trimmed, "#!/usr/bin/python") ||
		strings.Contains(c, "if __name__"):
		return &Lang{Name: "python", Ext: ".py", Cmd: []string{"python3"}}

	case strings.Contains(c, "console.log") ||
		strings.Contains(c, "const ") && strings.Contains(c, "=>") ||
		strings.Contains(c, "function ") && strings.Contains(c, "{"):
		return &Lang{Name: "javascript", Ext: ".js", Cmd: []string{"node"}}

	case strings.Contains(c, "puts ") || strings.Contains(c, "def ") && strings.Contains(c, "end"):
		if !strings.Contains(c, "print(") && strings.Contains(c, "end") {
			return &Lang{Name: "ruby", Ext: ".rb", Cmd: []string{"ruby"}}
		}
		return nil

	default:
		return nil
	}
}

// Run executes code content in a subprocess with the given timeout.
// The code is written to a temp file, executed, then cleaned up.
func Run(ctx context.Context, content string, timeout time.Duration) (*Result, error) {
	lang := DetectLang(content)
	if lang == nil {
		return nil, fmt.Errorf("cannot detect language — try adding a shebang line (e.g. #!/bin/bash)")
	}

	if timeout == 0 {
		timeout = DefaultTimeout
	}

	// Write to temp file
	tmpDir, err := os.MkdirTemp("", "cas-run-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	tmpFile := filepath.Join(tmpDir, "main"+lang.Ext)
	if err := os.WriteFile(tmpFile, []byte(content), 0600); err != nil {
		return nil, fmt.Errorf("write temp file: %w", err)
	}

	// Build command
	var args []string
	if lang.Compile != nil {
		args = append(lang.Compile, tmpFile)
	} else {
		args = append(lang.Cmd, tmpFile)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Dir = tmpDir

	// Platform-specific: process-group semantics differ (see proc_unix.go /
	// proc_windows.go). Ensures a timeout kills the child — and, on Unix,
	// its whole process tree.
	configureProcess(cmd)

	// Restrict environment — inherit PATH but not secrets
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + tmpDir,
		"TMPDIR=" + tmpDir,
		"LANG=en_US.UTF-8",
	}

	start := time.Now()
	err = cmd.Run()
	duration := time.Since(start)

	exitCode := 0
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return &Result{
				Stdout:   stdout.String(),
				Stderr:   fmt.Sprintf("execution timed out after %s", timeout),
				ExitCode: -1,
				Duration: duration,
				Language: lang.Name,
			}, nil
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("exec %s: %w", lang.Name, err)
		}
	}

	return &Result{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		Duration: duration,
		Language: lang.Name,
	}, nil
}

// FormatResult produces a human-readable summary for the chat panel.
func FormatResult(r *Result) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("ran %s (%s, exit %d)\n", r.Language, r.Duration.Round(time.Millisecond), r.ExitCode))

	if r.Stdout != "" {
		out := strings.TrimRight(r.Stdout, "\n")
		if len(out) > 4000 {
			out = out[:4000] + "\n… (truncated)"
		}
		b.WriteString(out)
	}

	if r.Stderr != "" {
		if r.Stdout != "" {
			b.WriteString("\n")
		}
		stderr := strings.TrimRight(r.Stderr, "\n")
		if len(stderr) > 2000 {
			stderr = stderr[:2000] + "\n… (truncated)"
		}
		b.WriteString("stderr: " + stderr)
	}

	if r.Stdout == "" && r.Stderr == "" {
		b.WriteString("(no output)")
	}

	return b.String()
}
