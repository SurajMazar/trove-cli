package bitwarden

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/redact"
)

// Runner executes an external command. stdin may be nil. Implementations must
// never include stdin in returned errors: it carries secret values.
type Runner interface {
	Run(ctx context.Context, stdin []byte, name string, args ...string) (stdout []byte, stderr []byte, err error)
}

// ExecRunner runs commands with os/exec. The child inherits the current
// process environment, which is how BW_SESSION and BWS_ACCESS_TOKEN reach the
// Bitwarden CLIs without ever appearing on a command line.
type ExecRunner struct{}

// maxStderr bounds how much of a command's stderr is kept in errors.
const maxStderr = 512

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	// Do not hang forever on grandchildren that keep the output pipes open
	// after the context is canceled.
	cmd.WaitDelay = 5 * time.Second

	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), stderr.Bytes(), nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, nil, fmt.Errorf("%s interrupted: %w", filepath.Base(name), ctxErr)
	}
	re := &RunError{Command: filepath.Base(name), ExitCode: -1, Err: err}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		re.ExitCode = ee.ExitCode()
		re.Err = nil
		re.Stderr = SanitizeStderr(stderr.String())
	}
	return stdout.Bytes(), stderr.Bytes(), re
}

// RunError describes a failed command without leaking its arguments (which
// may contain secret values for bws) or its standard input.
type RunError struct {
	Command  string // base name of the executable, e.g. "bw"
	ExitCode int    // -1 when the command could not be started
	Stderr   string // redacted and truncated standard error
	Err      error  // start failure, e.g. wrapping exec.ErrNotFound
}

func (e *RunError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("could not run %s: %v", e.Command, e.Err)
	}
	msg := fmt.Sprintf("%s exited with status %d", e.Command, e.ExitCode)
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	return msg
}

// Unwrap exposes start failures such as exec.ErrNotFound.
func (e *RunError) Unwrap() error { return e.Err }

// withoutSecret returns a copy of e whose stderr no longer contains secret
// (or any of its longer lines), as defense in depth against CLIs that echo
// their arguments in error messages.
func (e *RunError) withoutSecret(secret string) *RunError {
	cp := *e
	cp.Stderr = scrub(cp.Stderr, secret)
	return &cp
}

// SanitizeStderr prepares command stderr for inclusion in an error message:
// known token formats are redacted, whitespace is collapsed and the result is
// truncated.
func SanitizeStderr(s string) string {
	s = redact.String(s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxStderr {
		s = s[:maxStderr] + "..."
	}
	return s
}

// scrub removes secret, and each of its lines that is long enough to be
// meaningful, from s.
func scrub(s, secret string) string {
	if secret == "" || s == "" {
		return s
	}
	s = strings.ReplaceAll(s, secret, redact.Placeholder)
	// SanitizeStderr collapses whitespace, so also look for the collapsed form.
	if collapsed := strings.Join(strings.Fields(secret), " "); len(collapsed) >= 6 {
		s = strings.ReplaceAll(s, collapsed, redact.Placeholder)
	}
	for _, line := range strings.Split(secret, "\n") {
		line = strings.TrimSpace(line)
		if len(line) >= 6 {
			s = strings.ReplaceAll(s, line, redact.Placeholder)
		}
	}
	return s
}
