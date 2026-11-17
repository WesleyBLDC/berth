// Package logging wraps slog so attributes that look like secrets are never written.
package logging

import (
	"context"
	"log/slog"
	"strings"
)

var secretWords = []string{"secret", "token", "password", "passwd", "credential", "apikey", "api_key", "authorization", "cookie"}

const redacted = "[redacted]"

// Redacting replaces the value of any attribute whose key names a secret.
type Redacting struct {
	next slog.Handler
}

func NewRedacting(next slog.Handler) *Redacting { return &Redacting{next: next} }

func (h *Redacting) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *Redacting) Handle(ctx context.Context, r slog.Record) error {
	clean := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		clean.AddAttrs(redact(a))
		return true
	})
	return h.next.Handle(ctx, clean)
}

func (h *Redacting) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = redact(a)
	}
	return &Redacting{next: h.next.WithAttrs(out)}
}

func (h *Redacting) WithGroup(name string) slog.Handler {
	return &Redacting{next: h.next.WithGroup(name)}
}

func redact(a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		group := a.Value.Group()
		out := make([]any, len(group))
		for i, g := range group {
			out[i] = redact(g)
		}
		return slog.Group(a.Key, out...)
	}
	key := strings.ToLower(a.Key)
	for _, w := range secretWords {
		if strings.Contains(key, w) {
			return slog.String(a.Key, redacted)
		}
	}
	return a
}
