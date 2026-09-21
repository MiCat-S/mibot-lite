// Package command is the registry and dispatcher: it parses a prefix and a
// command name out of an outgoing message and runs the handler on its own
// goroutine with a timeout.
package command

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/bot"
)

// Invocation is one parsed command.
type Invocation struct {
	Prefix  string
	Command string
	Args    []string
	Text    string
	Message *bot.Message
	Client  *bot.Client
	Log     *slog.Logger
}

// Arg returns the i-th argument or "".
func (inv *Invocation) Arg(index int) string {
	if index < len(inv.Args) {
		return inv.Args[index]
	}
	return ""
}

// Rest joins the arguments from index on.
func (inv *Invocation) Rest(index int) string {
	if index >= len(inv.Args) {
		return ""
	}
	return strings.TrimSpace(strings.Join(inv.Args[index:], " "))
}

// Edit replaces the command message with HTML.
func (inv *Invocation) Edit(ctx context.Context, html string) error {
	return inv.Client.Edit(ctx, inv.Message, html)
}

// EditText replaces the command message with literal text.
func (inv *Invocation) EditText(ctx context.Context, text string) error {
	return inv.Client.EditText(ctx, inv.Message, text)
}

// Reply answers the command message with HTML.
func (inv *Invocation) Reply(ctx context.Context, html string) error {
	_, err := inv.Client.Reply(ctx, inv.Message, html)
	return err
}

// Command is one registered command.
type Command struct {
	Name        string
	Description string
	// Usage is the argument summary shown in the command list.
	Usage string
	// Help renders the long help for `.help name`. Nil falls back to the
	// description.
	Help func(prefix string) string
	// Handle runs the command. Errors are logged and reported to the chat.
	Handle func(ctx context.Context, inv *Invocation) error
	// Timeout bounds the handler. Zero means the default (5 minutes);
	// negative means no timeout at all.
	Timeout time.Duration
	// Hidden keeps the command out of the list (aliases).
	Hidden bool
}

// Job is background work started once the account is connected.
type Job func(ctx context.Context, client *bot.Client)

// Registry holds the commands and prefixes.
type Registry struct {
	mu       sync.RWMutex
	commands map[string]*Command
	prefixes []string
	jobs     []Job
	logger   *slog.Logger
	sem      chan struct{}
	inFlight sync.WaitGroup
}

// New builds an empty registry.
func New(prefixes []string, logger *slog.Logger) *Registry {
	if len(prefixes) == 0 {
		prefixes = []string{"."}
	}
	return &Registry{commands: map[string]*Command{}, prefixes: prefixes, logger: logger, sem: make(chan struct{}, 16)}
}

// Register adds commands; a duplicate name is a programming error.
func (r *Registry) Register(commands ...*Command) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, command := range commands {
		if command == nil || !commandName.MatchString(command.Name) {
			panic(fmt.Sprintf("invalid command %q", command.Name))
		}
		if _, exists := r.commands[command.Name]; exists {
			panic("duplicate command " + command.Name)
		}
		r.commands[command.Name] = command
	}
}

// AddJob schedules background work for after connection.
func (r *Registry) AddJob(job Job) {
	r.mu.Lock()
	r.jobs = append(r.jobs, job)
	r.mu.Unlock()
}

// Jobs returns the registered background work.
func (r *Registry) Jobs() []Job {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Job(nil), r.jobs...)
}

// Commands lists the visible commands, sorted by name.
func (r *Registry) Commands() []*Command {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := make([]*Command, 0, len(r.commands))
	for _, command := range r.commands {
		if !command.Hidden {
			list = append(list, command)
		}
	}
	sort.Slice(list, func(a, b int) bool { return list[a].Name < list[b].Name })
	return list
}

// Lookup finds a command by name.
func (r *Registry) Lookup(name string) (*Command, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	command, ok := r.commands[name]
	return command, ok
}

// Prefixes returns the active prefixes.
func (r *Registry) Prefixes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.prefixes...)
}

// Prefix returns the primary prefix, for help text.
func (r *Registry) Prefix() string { return r.Prefixes()[0] }

var commandName = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// Route is a parsed command line.
type Route struct {
	Prefix  string
	Command string
	Args    []string
}

// Parse resolves a message's text to a route: the longest matching prefix
// wins, the first token is the command, the rest are arguments.
func (r *Registry) Parse(text string) (Route, bool) {
	prefixes := r.Prefixes()
	prefix, matched := "", false
	for _, candidate := range prefixes {
		if strings.HasPrefix(text, candidate) && (!matched || len(candidate) > len(prefix)) {
			prefix, matched = candidate, true
		}
	}
	if !matched {
		return Route{}, false
	}
	parts := strings.FieldsFunc(strings.TrimFunc(text[len(prefix):], isSpace), isSpace)
	if len(parts) == 0 || !commandName.MatchString(parts[0]) {
		return Route{}, false
	}
	return Route{Prefix: prefix, Command: parts[0], Args: append([]string{}, parts[1:]...)}, true
}

func isSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// Dispatch offers a message to the registry. It reports whether a command
// matched; the handler runs asynchronously.
func (r *Registry) Dispatch(ctx context.Context, client *bot.Client, message *bot.Message) bool {
	route, ok := r.Parse(message.Text)
	if !ok {
		return false
	}
	command, ok := r.Lookup(route.Command)
	if !ok {
		return false
	}
	inv := &Invocation{Prefix: route.Prefix, Command: route.Command, Args: route.Args, Text: message.Text,
		Message: message, Client: client, Log: r.logger.With(slog.String("command", route.Command))}
	r.inFlight.Add(1)
	go func() {
		defer r.inFlight.Done()
		select {
		case r.sem <- struct{}{}:
			defer func() { <-r.sem }()
		case <-ctx.Done():
			return
		}
		runCtx, cancel := ctx, context.CancelFunc(func() {})
		switch {
		case command.Timeout < 0:
		case command.Timeout == 0:
			runCtx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		default:
			runCtx, cancel = context.WithTimeout(ctx, command.Timeout)
		}
		defer cancel()
		defer func() {
			if recovered := recover(); recovered != nil {
				inv.Log.Error("command.panic", slog.Any("panic", recovered))
				_ = inv.EditText(context.WithoutCancel(ctx), "命令执行时发生内部错误")
			}
		}()
		started := time.Now()
		err := command.Handle(runCtx, inv)
		switch {
		case err == nil:
			inv.Log.Info("command.handled", slog.String("chat", message.ChatID), slog.Int("message", message.ID), slog.Duration("took", time.Since(started)))
		case ctx.Err() != nil:
		case errors.Is(err, context.DeadlineExceeded):
			inv.Log.Warn("command.timeout", slog.String("chat", message.ChatID))
			_ = inv.EditText(context.WithoutCancel(ctx), "命令执行超时")
		default:
			inv.Log.Error("command.failed", slog.String("chat", message.ChatID), slog.Int("message", message.ID), slog.String("error", err.Error()))
			_ = inv.EditText(context.WithoutCancel(ctx), "命令执行失败："+Brief(err))
		}
	}()
	return true
}

// Wait blocks until in-flight commands finish or the timeout passes.
func (r *Registry) Wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() { r.inFlight.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Brief renders an error for a chat: the Telegram RPC code when there is
// one, else a short generic line, never a URL or a host.
func Brief(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if match := rpcCode.FindString(text); match != "" {
		return match
	}
	if errors.Is(err, bot.ErrUnaddressablePeer) {
		return "无法定位目标会话"
	}
	if len(text) > 80 {
		text = text[:80]
	}
	return text
}

var rpcCode = regexp.MustCompile(`\b[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+\b`)
