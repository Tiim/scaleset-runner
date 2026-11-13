// Package logging provides the Controller's JSON logger, which removes every
// registered secret (credentials, private key, JIT Configs, message queue
// tokens) from messages and attributes before they are written (NFR-007).
package logging

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
)

const (
	placeholder = "[REDACTED]"
	// minSecretLen keeps short values from redacting ordinary words.
	minSecretLen = 8
)

// Redactor holds the secret values that must never reach a log line.
type Redactor struct {
	mu       sync.RWMutex
	secrets  map[string]int
	replacer *strings.Replacer
}

func NewRedactor() *Redactor {
	return &Redactor{secrets: map[string]int{}}
}

// Add registers a secret. Multi-line secrets (a PEM key) are also registered
// line by line, so a fragment quoted in an error is caught as well.
func (r *Redactor) Add(secret string) {
	r.change(secret, 1)
}

// Remove unregisters a secret added before, e.g. a consumed JIT Config.
func (r *Redactor) Remove(secret string) {
	r.change(secret, -1)
}

func (r *Redactor) change(secret string, delta int) {
	parts := []string{secret}
	if strings.Contains(secret, "\n") {
		for _, line := range strings.Split(secret, "\n") {
			parts = append(parts, strings.TrimSpace(line))
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range parts {
		if len(p) < minSecretLen || strings.HasPrefix(p, "-----") {
			continue
		}
		r.secrets[p] += delta
		if r.secrets[p] <= 0 {
			delete(r.secrets, p)
		}
	}
	r.rebuild()
}

func (r *Redactor) rebuild() {
	if len(r.secrets) == 0 {
		r.replacer = nil
		return
	}
	keys := make([]string, 0, len(r.secrets))
	for k := range r.secrets {
		keys = append(keys, k)
	}
	// Longest first, so a whole secret wins over one of its lines.
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	pairs := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		pairs = append(pairs, k, placeholder)
	}
	r.replacer = strings.NewReplacer(pairs...)
}

// Redact returns s with every registered secret replaced.
func (r *Redactor) Redact(s string) string {
	r.mu.RLock()
	rep := r.replacer
	r.mu.RUnlock()
	if rep == nil {
		return s
	}
	return rep.Replace(s)
}

// NewLogger returns a JSON logger writing to w that redacts through r.
func NewLogger(w io.Writer, level slog.Level, r *Redactor) *slog.Logger {
	return slog.New(&handler{inner: slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}), r: r})
}

type handler struct {
	inner slog.Handler
	r     *Redactor
}

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool { return h.inner.Enabled(ctx, l) }

func (h *handler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, h.r.Redact(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.attr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = h.attr(a)
	}
	return &handler{inner: h.inner.WithAttrs(red), r: h.r}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{inner: h.inner.WithGroup(name), r: h.r}
}

func (h *handler) attr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, h.r.Redact(v.String()))
	case slog.KindGroup:
		group := v.Group()
		red := make([]any, len(group))
		for i, g := range group {
			red[i] = h.attr(g)
		}
		return slog.Group(a.Key, red...)
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return slog.String(a.Key, h.r.Redact(x.Error()))
		case []byte:
			return slog.String(a.Key, h.r.Redact(string(x)))
		case interface{ String() string }:
			return slog.String(a.Key, h.r.Redact(x.String()))
		default:
			// Anything else is marshalled by the JSON handler; render it as
			// text first so nothing escapes redaction.
			return slog.String(a.Key, h.r.Redact(slog.AnyValue(x).String()))
		}
	default:
		return slog.Attr{Key: a.Key, Value: v}
	}
}
