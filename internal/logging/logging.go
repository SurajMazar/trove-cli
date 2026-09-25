// Package logging configures structured logging (log/slog) with mandatory
// redaction of tokens, passwords, cookies, authorization headers and secret
// references.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/SurajMazar/trove-cli/internal/redact"
)

// ParseLevel parses debug|info|warn|error|off. Empty means off (quiet
// default: Trove prints nothing but results and errors).
func ParseLevel(s string) (slog.Level, bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off", "none", "quiet":
		return 0, false, nil
	case "debug":
		return slog.LevelDebug, true, nil
	case "info":
		return slog.LevelInfo, true, nil
	case "warn", "warning":
		return slog.LevelWarn, true, nil
	case "error":
		return slog.LevelError, true, nil
	}
	return 0, false, fmt.Errorf("invalid log level %q (want debug, info, warn, error or off)", s)
}

// New returns a logger writing text records to w at level, or a discarding
// logger when enabled is false.
func New(w io.Writer, level slog.Level, enabled bool) *slog.Logger {
	if !enabled {
		return slog.New(slog.DiscardHandler)
	}
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: level, ReplaceAttr: replaceAttr})
	return slog.New(&redactingHandler{h})
}

var sensitiveAttr = []string{"token", "password", "secret", "authorization", "cookie", "credential", "private_key", "session"}

func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	key := strings.ToLower(a.Key)
	if key == "secret_ref" {
		// References are not secrets, but they reveal vault layout; keep
		// only the provider scheme.
		if s, ok := a.Value.Any().(string); ok {
			if i := strings.Index(s, "://"); i > 0 {
				return slog.String(a.Key, s[:i]+"://"+redact.Placeholder)
			}
		}
		return slog.String(a.Key, redact.Placeholder)
	}
	for _, s := range sensitiveAttr {
		if strings.Contains(key, s) {
			return slog.String(a.Key, redact.Placeholder)
		}
	}
	if a.Value.Kind() == slog.KindString {
		return slog.String(a.Key, redact.String(a.Value.String()))
	}
	return a
}

// redactingHandler additionally scrubs the log message itself.
type redactingHandler struct{ slog.Handler }

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, redact.String(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(a)
		return true
	})
	return h.Handler.Handle(ctx, nr)
}

func (h *redactingHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &redactingHandler{h.Handler.WithAttrs(as)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{h.Handler.WithGroup(name)}
}
