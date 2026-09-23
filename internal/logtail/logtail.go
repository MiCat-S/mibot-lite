// Package logtail 把最近的日志行留在内存里，以便导出成文件交给别人。
//
// 服务器上的 journal 什么都有，但让用户去跑 journalctl 再把输出贴出来，
// 等于让他们泄露自己的聊天 ID。这里产出的内容本来就是要转发出去的：
// 它在写入时脱敏，而不是在读出时脱敏，因为到输出时才脱敏的话，
// 漏掉一个调用点就是一次泄露。
//
// 它活不过进程。一重启就清空，所以崩溃的原因仍然要去 journal 里查。
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
	// 保留的行数。多到关键的部分通常还在里面，又少到整个环形缓冲区
	// 远不到 100 KB。
	Lines = 300
	// 每行都有长度上限，免得一条超长的记录把环形缓冲区撑大。
	LineLimit = 240
)

// identifying 里的键指向某个人或某个聊天。它们的值会换成一个稳定的
// 短摘要，这样在不同的行里仍能看出是同一个聊天，但看不出是哪一个。
var identifying = map[string]bool{
	"account": true, "chat": true, "chat_id": true, "channel": true,
	"channel_id": true, "peer": true, "sender": true, "user": true,
	"user_id": true, "from": true, "to": true, "target": true,
}

// carrying 里的键装的是内容或凭据，一点都不保留：消息正文和 API key
// 不管处理成什么样，都不能放心转发。
var carrying = map[string]bool{
	"text": true, "query": true, "prompt": true, "caption": true,
	"title": true, "username": true, "phone": true, "key": true,
	"api_key": true, "token": true, "secret": true, "password": true,
	"url": true, "link": true, "path": true, "file": true,
}

var (
	addresses = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	// 单独出现的九位及以上数字，总归是某种 id。
	longIDs = regexp.MustCompile(`\b\d{9,}\b`)
	tokens  = regexp.MustCompile(`\b(?:sk|xox[a-z]|ghp|gho|Bearer)[-_ ][A-Za-z0-9_.\-]{8,}`)
	// URL 只留协议和主机，其余去掉：主机能说明是哪个服务出的错，
	// 路径则是藏 key 的地方。
	urls = regexp.MustCompile(`(https?://[^/\s"]+)(/[^\s"]*)?`)
)

// Scrub 从自由文本里去掉不该离开本机的内容。
//
// 它处理的是本包无法按键名归类的值，实际上就是错误信息：这是日志里
// 最有用的部分，也最可能夹带地址或 URL。
func Scrub(value string) string {
	value = urls.ReplaceAllString(value, "$1/…")
	value = tokens.ReplaceAllString(value, "[token]")
	value = addresses.ReplaceAllString(value, "[ip]")
	value = longIDs.ReplaceAllString(value, "[id]")
	return value
}

// Digest 为一个标识符生成稳定的短代号。
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

// Ring 是存放已格式化日志行的定长环形缓冲区。
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

// Tail 返回最多 count 行，旧的在前，只保留过滤函数接受的行。
// 过滤函数为 nil 时全部保留；count <= 0 时不限行数。
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

// Held 返回环形缓冲区当前存了多少行。
func (r *Ring) Held() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.full {
		return len(r.lines)
	}
	return r.next
}

// Handler 把经它转交的每条记录脱敏后，复制一份到环形缓冲区。
//
// 它包在真正的 handler 外面，而不是取代它，所以 journal 收到的内容
// 和以前完全一样。
type Handler struct {
	next   slog.Handler
	ring   *Ring
	prefix string
}

// Wrap 返回一个 handler：照常写给 next，同时记下最近的日志。
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

// AtLeast 判断一行已格式化日志的级别是否达到 floor。级别是该行的
// 第二个字段。
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
