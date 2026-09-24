// Package command 是命令的注册表和分发器：从发出的消息里解析出前缀和
// 命令名，在单独的 goroutine 里带超时运行对应的处理函数。
package command

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/MiCat-S/mibot-lite/internal/bot"
)

// Invocation 是解析好的一次命令调用。
type Invocation struct {
	Prefix  string
	Command string
	Args    []string
	Text    string
	Message *bot.Message
	Client  *bot.Client
	Log     *slog.Logger
	// Trigger 不为空时，这条命令是别人借用账号发起的（.sudo、.sure）：
	// Message 是账号代发的那条，Trigger 是对方原来发的那条。
	Trigger *bot.Message
}

// Arg 返回第 i 个参数，没有就返回 ""。
func (inv *Invocation) Arg(index int) string {
	if index < len(inv.Args) {
		return inv.Args[index]
	}
	return ""
}

// Rest 把从 index 开始的参数拼起来。
func (inv *Invocation) Rest(index int) string {
	if index >= len(inv.Args) {
		return ""
	}
	return strings.TrimSpace(strings.Join(inv.Args[index:], " "))
}

// Edit 把命令消息改成 HTML 内容。
func (inv *Invocation) Edit(ctx context.Context, html string) error {
	return inv.Client.Edit(ctx, inv.Message, html)
}

// EditText 把命令消息改成纯文本，原样显示。
func (inv *Invocation) EditText(ctx context.Context, text string) error {
	return inv.Client.EditText(ctx, inv.Message, text)
}

// Reply 用 HTML 回复命令消息。
func (inv *Invocation) Reply(ctx context.Context, html string) error {
	_, err := inv.Client.Reply(ctx, inv.Message, html)
	return err
}

// Command 是一个已注册的命令。
type Command struct {
	Name        string
	Description string
	// Usage 是命令列表里显示的参数摘要。
	Usage string
	// Help 生成 `.help name` 显示的详细帮助。为 nil 时改用 Description。
	Help func(prefix string) string
	// Handle 执行命令。返回的错误会写进日志，并报告到聊天里。
	Handle func(ctx context.Context, inv *Invocation) error
	// Timeout 是处理函数的时限。0 表示用默认值（5 分钟）；
	// 负数表示完全不设时限。
	Timeout time.Duration
	// Hidden 让命令不出现在列表里（用于别名）。
	Hidden bool
}

// HelpText 是这条命令的详细帮助：有 Help 用 Help，否则用用法加说明。
func (c *Command) HelpText(prefix string) string {
	if c.Help != nil {
		return c.Help(prefix)
	}
	usage := prefix + c.Name
	if c.Usage != "" {
		usage += " " + c.Usage
	}
	return "<b>" + Escape(usage) + "</b>\n\n" + Escape(c.Description)
}

// wantsHelp 判断参数是不是只有一个 --help。
func wantsHelp(args []string) bool { return len(args) == 1 && args[0] == "--help" }

// Job 是账号连上之后启动的后台任务。
type Job func(ctx context.Context, client *bot.Client)

// Registry 保存命令和前缀。
type Registry struct {
	mu       sync.RWMutex
	commands map[string]*Command
	// aliases 把使用者自己起的名字映射到它代表的命令行，参数也包括在内。
	// 由 .alias 修改。
	aliases  map[string]string
	prefixes []string
	jobs     []Job
	logger   *slog.Logger
	sem      chan struct{}
	inFlight sync.WaitGroup
	// relayed 是账号替别人代发过的命令消息。它们已经按借用规则执行过一次；
	// 万一之后又被当成本人发的消息收到，不能不经检查再执行一遍。
	relayed map[string]time.Time
}

// New 创建一个空的注册表。
func New(prefixes []string, logger *slog.Logger) *Registry {
	if len(prefixes) == 0 {
		prefixes = []string{"."}
	}
	return &Registry{commands: map[string]*Command{}, prefixes: prefixes, logger: logger, sem: make(chan struct{}, 16)}
}

// Register 添加命令；名字重复属于编程错误。
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

// AddJob 登记一项连上之后再执行的后台任务。
func (r *Registry) AddJob(job Job) {
	r.mu.Lock()
	r.jobs = append(r.jobs, job)
	r.mu.Unlock()
}

// Jobs 返回已登记的后台任务。
func (r *Registry) Jobs() []Job {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Job(nil), r.jobs...)
}

// Commands 列出可见的命令，按名字排序。
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

// Lookup 按名字查找命令。
func (r *Registry) Lookup(name string) (*Command, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	command, ok := r.commands[name]
	return command, ok
}

// Prefixes 返回当前生效的前缀。
func (r *Registry) Prefixes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.prefixes...)
}

// SetAliases 替换整张别名表。
func (r *Registry) SetAliases(aliases map[string]string) {
	copied := make(map[string]string, len(aliases))
	for name, target := range aliases {
		copied[name] = target
	}
	r.mu.Lock()
	r.aliases = copied
	r.mu.Unlock()
}

// Aliases 返回别名表的副本。
func (r *Registry) Aliases() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	copied := make(map[string]string, len(r.aliases))
	for name, target := range r.aliases {
		copied[name] = target
	}
	return copied
}

// SetPrefixes 在运行中换掉前缀，之后收到的消息立刻按新前缀解析。
// 空列表会被忽略：没有前缀，就再也发不出能改回来的命令了。
func (r *Registry) SetPrefixes(prefixes []string) {
	if len(prefixes) == 0 {
		return
	}
	r.mu.Lock()
	r.prefixes = append([]string(nil), prefixes...)
	r.mu.Unlock()
}

// Prefix 返回主前缀，供帮助文本使用。
func (r *Registry) Prefix() string { return r.Prefixes()[0] }

var commandName = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// Route 是解析好的命令行。
type Route struct {
	Prefix  string
	Command string
	Args    []string
	// Text 是命令应当读到的消息文本。对别名来说，它是展开后的内容，
	// 后面接上别名之后的所有内容，一字不差地保留原样：.gt 和 .yvlu
	// 从原始文本读取输入，换行也算在内，把拆开的词重新拼起来会丢掉换行。
	Text string
}

// Parse 把消息文本解析成路由：先取最长的匹配前缀，再取最长的匹配别名，
// 然后第一个词是命令，其余的是参数。
//
// 单个词的别名永远不会盖住同名的真实命令，MiBox 也是这个规则；
// 多个词的别名可以以命令名开头。
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
	body := text[len(prefix):]
	parts := strings.FieldsFunc(strings.TrimFunc(body, isSpace), isSpace)
	if len(parts) == 0 {
		return Route{}, false
	}
	r.mu.RLock()
	aliases := r.aliases
	_, isCommand := r.commands[parts[0]]
	r.mu.RUnlock()
	for length := len(parts); length > 0 && len(aliases) > 0; length-- {
		if length == 1 && isCommand {
			break
		}
		expansion, ok := aliases[strings.Join(parts[:length], " ")]
		if !ok {
			continue
		}
		expanded := strings.FieldsFunc(expansion, isSpace)
		if len(expanded) == 0 || !commandName.MatchString(expanded[0]) {
			return Route{}, false
		}
		args := append(append([]string{}, expanded[1:]...), parts[length:]...)
		return Route{Prefix: prefix, Command: expanded[0], Args: args,
			Text: prefix + strings.Join(expanded, " ") + afterTokens(body, length)}, true
	}
	if !commandName.MatchString(parts[0]) {
		return Route{}, false
	}
	return Route{Prefix: prefix, Command: parts[0], Args: append([]string{}, parts[1:]...), Text: text}, true
}

// afterTokens 去掉 s 开头 n 个以空白分隔的词，剩下的部分原样返回，
// 开头的空白也保留。
func afterTokens(s string, n int) string {
	index := 0
	for count := 0; count < n; count++ {
		for index < len(s) {
			r, size := utf8.DecodeRuneInString(s[index:])
			if !isSpace(r) {
				break
			}
			index += size
		}
		for index < len(s) {
			r, size := utf8.DecodeRuneInString(s[index:])
			if isSpace(r) {
				break
			}
			index += size
		}
	}
	return s[index:]
}

func isSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// Dispatch 把一条消息交给注册表，返回是否匹配到命令；
// 处理函数异步运行。
func (r *Registry) Dispatch(ctx context.Context, client *bot.Client, message *bot.Message) bool {
	if r.wasRelayed(message) {
		r.logger.Info("dispatch.relayed_again", slog.String("chat", message.ChatID), slog.Int("message", message.ID))
		return false
	}
	route, ok := r.Parse(message.Text)
	if !ok {
		return false
	}
	command, ok := r.Lookup(route.Command)
	if !ok {
		return false
	}
	r.run(ctx, client, message, nil, route, command)
	return true
}

// DispatchFor 执行一条账号替别人代发的命令。allowed 决定这条命令能不能借出去，
// 不许就不执行；trigger 是对方原来的那条消息。
func (r *Registry) DispatchFor(ctx context.Context, client *bot.Client, message, trigger *bot.Message, allowed func(Route) bool) bool {
	route, ok := r.Parse(message.Text)
	if !ok || !allowed(route) {
		return false
	}
	command, ok := r.Lookup(route.Command)
	if !ok {
		return false
	}
	r.markRelayed(message)
	r.run(ctx, client, message, trigger, route, command)
	return true
}

func relayKey(message *bot.Message) string { return message.ChatID + "/" + strconv.Itoa(message.ID) }

// markRelayed 记下一条代发的消息。记录保留 10 分钟，足够覆盖更新晚到的情况。
func (r *Registry) markRelayed(message *bot.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.relayed == nil {
		r.relayed = map[string]time.Time{}
	}
	now := time.Now()
	for key, at := range r.relayed {
		if now.Sub(at) > 10*time.Minute {
			delete(r.relayed, key)
		}
	}
	r.relayed[relayKey(message)] = now
}

func (r *Registry) wasRelayed(message *bot.Message) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.relayed[relayKey(message)]
	return ok
}

// run 在后台执行一条命令：限制同时运行的数量、套上超时、兜住 panic、记日志。
func (r *Registry) run(ctx context.Context, client *bot.Client, message, trigger *bot.Message, route Route, command *Command) {
	logger := r.logger.With(slog.String("command", route.Command))
	if trigger != nil {
		logger = logger.With(slog.String("for", trigger.ChatID+"#"+strconv.Itoa(trigger.ID)))
	}
	inv := &Invocation{Prefix: route.Prefix, Command: route.Command, Args: route.Args, Text: route.Text,
		Message: message, Client: client, Log: logger, Trigger: trigger}
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
		var err error
		if wantsHelp(inv.Args) {
			// 和 MiBox 一样，「命令 --help」只显示帮助、不执行；有的命令根本不看参数，
			// 不拦下来的话 .restart --help 就真的重启了。
			err = inv.Edit(runCtx, command.HelpText(inv.Prefix))
		} else {
			err = command.Handle(runCtx, inv)
		}
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
}

// Wait 阻塞到正在运行的命令全部结束，或者超时为止。
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

// Brief 把错误转成适合发到聊天里的文字：有 Telegram RPC 错误码就只给
// 错误码，否则给一句简短的通用说明，绝不带 URL 或主机名。
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
