// Package logtail keeps the recent log lines in memory so they can be
// exported as a file and handed to someone else.
//
// The journal on the server has everything, but asking a user to run
// journalctl and paste the output is asking them to leak their own chat
// ids. What this produces is meant to be forwarded: it is scrubbed as it
// is written, not as it is read, because scrubbing on the way out means
// one forgotten call site is a leak.
//
// It does not survive the process. A restart empties it, so a crash is
// still a question for the journal.
package logtail

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// Lines held. Enough that the interesting part is usually still in
	// there, small enough that the whole ring is well under 100 KB.
	Lines = 300
	// Each line is capped so one enormous record cannot grow the ring.
	LineLimit = 240
)

// identifying keys name something that points at a person or a chat.
// Their values are replaced by a short stable digest, so the same chat
// still reads as the same chat across lines without saying which one.
var identifying = map[string]bool{
	"account": true, "chat": true, "chat_id": true, "channel": true,
	"channel_id": true, "peer": true, "sender": true, "user": true,
	"user_id": true, "from": true, "to": true, "target": true,
}

// carrying keys hold content or credentials. Nothing of them survives:
// there is no version of a message body or an API key that is safe to
// forward.
var carrying = map[string]bool{
	"text": true, "query": true, "prompt": true, "caption": true,
	"title": true, "username": true, "phone": true, "key": true,
	"api_key": true, "token": true, "secret": true, "password": true,
	"url": true, "link": true, "path": true, "file": true,
}

var (
	addresses = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	// Nine digits or more, standing alone, is an id of some kind.
	longIDs = regexp.MustCompile(`\b\d{9,}\b`)
	tokens  = regexp.MustCompile(`\b(?:sk|xox[a-z]|ghp|gho|Bearer)[-_ ][A-Za-z0-9_.\-]{8,}`)
	// Keep the scheme and host of a URL, drop the rest: the host says
	// which service failed, the path is where keys hide.
	urls = regexp.MustCompile(`(https?://[^/\s"]+)(/[^\s"]*)?`)
)

// Scrub removes what should not leave the machine from free text.
//
// It runs over values this package cannot classify by key, which in
// practice means error strings — the most useful part of a log and the
// one most likely to have an address or a URL embedded in it.
func Scrub(value string) string {
	value = urls.ReplaceAllString(value, "$1/…")
	value = tokens.ReplaceAllString(value, "[token]")
	value = addresses.ReplaceAllString(value, "[ip]")
	value = longIDs.ReplaceAllString(value, "[id]")
	return value
}

// Digest is the stable short stand-in for an identifier.
func Digest(value string) string {
	if value == "" {
		return "#none"
	}
	sum := fnv.New32a()
	_, _ = sum.Write([]byte(value))
	return fmt.Sprintf("#%04x", sum.Sum32()&0xffff)
}

func redact(key, value string) string {
	switch {
	case identifying[key]:
		return Digest(value)
	case carrying[key]:
		return "[redacted]"
	}
	return Scrub(value)
}

// Ring is a fixed-size circular buffer of rendered log lines.
type Ring struct {
	mu    sync.Mutex
	lines []string
	next  int
	full  bool
}

func New() *Ring { return &Ring{lines: make([]string, Lines)} }

func (r *Ring) add(line string) {
	if runes := []rune(line); len(runes) > LineLimit {
		line = string(runes[:LineLimit]) + "…"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines[r.next] = line
	r.next = (r.next + 1) % len(r.lines)
	if r.next == 0 {
		r.full = true
	}
}

// Tail returns at most count lines, oldest first, keeping only those the
// filter accepts. A nil filter keeps everything; count <= 0 keeps all.
func (r *Ring) Tail(count int, keep func(string) bool) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	held := r.next
	start := 0
	if r.full {
		held, start = len(r.lines), r.next
	}
	matched := make([]string, 0, held)
	for i := 0; i < held; i++ {
		line := r.lines[(start+i)%len(r.lines)]
		if keep == nil || keep(line) {
			matched = append(matched, line)
		}
	}
	if count > 0 && len(matched) > count {
		matched = matched[len(matched)-count:]
	}
	return matched
}

// Held reports how many lines the ring currently holds.
func (r *Ring) Held() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.full {
		return len(r.lines)
	}
	return r.next
}

// Handler copies every record it passes on into a ring, redacted.
//
// It wraps rather than replaces the real handler, so the journal keeps
// receiving exactly what it received before.
type Handler struct {
	next   slog.Handler
	ring   *Ring
	prefix string
}

// Wrap returns a handler that writes to next and remembers the tail.
func Wrap(next slog.Handler, ring *Ring) *Handler {
	return &Handler{next: next, ring: ring}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, record slog.Record) error {
	stamp := record.Time
	if stamp.IsZero() {
		stamp = time.Now()
	}
	var line strings.Builder
	line.WriteString(stamp.Format("15:04:05"))
	line.WriteByte(' ')
	line.WriteString(record.Level.String())
	line.WriteByte(' ')
	line.WriteString(record.Message)
	line.WriteString(h.prefix)
	record.Attrs(func(attr slog.Attr) bool {
		line.WriteString(pair(attr))
		return true
	})
	h.ring.add(line.String())
	return h.next.Handle(ctx, record)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{next: h.next.WithAttrs(attrs), ring: h.ring, prefix: h.prefix + render(attrs)}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{next: h.next.WithGroup(name), ring: h.ring, prefix: h.prefix}
}

func pair(attr slog.Attr) string {
	return " " + attr.Key + "=" + redact(attr.Key, attr.Value.String())
}

func render(attrs []slog.Attr) string {
	var out strings.Builder
	for _, attr := range attrs {
		out.WriteString(pair(attr))
	}
	return out.String()
}

// AtLeast reports whether a rendered line's level reaches floor. The
// level is the line's second field.
func AtLeast(line string, floor slog.Level) bool {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return false
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(fields[1])); err != nil {
		return false
	}
	return level >= floor
}
