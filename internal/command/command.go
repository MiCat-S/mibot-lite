// Package command 是命令的注册表和分发器：从发出的消息里解析出前缀和
// 命令名，在有界工作池里带超时运行对应的处理函数。
package command

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gotd/td/tg"

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

// RawAfter 返回命令名后面、再跳过 n 个词之后的原文，空白和换行照原样保留（包括开头的空白）。
// 要读原样正文的命令（翻译、提问、伪造语录、多行添加）用它，不要把 Args 拼回去：那样换行就丢了。
func (inv *Invocation) RawAfter(n int) string {
	return afterTokens(strings.TrimPrefix(inv.Text, inv.Prefix), 1+n)
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

// EditRich 有 Premium 时把命令消息改成 Rich Message；发不了、超出上限或者被拒收时
// 改成 fallback 这段 HTML。
func (inv *Invocation) EditRich(ctx context.Context, blocks []tg.PageBlockClass, fallback string) error {
	if inv.Client.CanSendRich() && bot.RichFits(blocks) {
		peer, err := inv.Client.InputPeer(inv.Message.Peer)
		if err != nil {
			return err
		}
		err = inv.Client.EditRich(ctx, peer, inv.Message.ID, blocks)
		if !bot.RichRejected(err) {
			return err
		}
		inv.Log.Warn("command.rich_rejected", "error", err.Error())
	}
	return inv.Edit(ctx, fallback)
}

// EditPages 把第一页改进命令消息，其余各页作为回复接着发。
func (inv *Invocation) EditPages(ctx context.Context, pages []string) error {
	for index, page := range pages {
		if err := ctx.Err(); err != nil {
			return err
		}
		if index == 0 {
			if err := inv.Edit(ctx, page); err != nil {
				return err
			}
			continue
		}
		if err := inv.Reply(ctx, page); err != nil {
			return err
		}
	}
	return nil
}

// Command 是一个已注册的命令。
type Command struct {
	Name string
	// Aliases 是这条命令的其他名字（简写 st、全称 autochangename 之类），和 Name 一样能调用。
	// 列表里只显示在 Name 那一行，借用规则、日志都按 Name 算。
	Aliases []string
	// Group 是 .help 列表和 README 表格里的分组，取值是 Groups 里的一个。
	Group string
	// Description 是列表里的一句话说明：动宾短语，不带从句和句号。
	Description string
	// Usage 是参数的形状，如 "[目标] [时长]"：[] 里可以省略，a|b 是几选一。
	// 子命令多的只写最常用的形状，其余由 Help 说明。
	Usage string
	// Help 生成 `.help name` 显示的详细帮助。为 nil 时改用用法加 Description。
	Help func(prefix string) string
	// Handle 执行命令。返回的错误会写进日志，并报告到聊天里。
	Handle func(ctx context.Context, inv *Invocation) error
	// Timeout 是处理函数的时限。0 表示用默认值（5 分钟），NoTimeout 表示完全不设时限。
	Timeout time.Duration
	// FreeText 表示第一个参数可能就是正文（要翻译的词、算式、问题）。「命令 help」「命令 h」
	// 平时由派发器直接显示帮助，这类命令交给处理函数自己分辨。「命令 --help」对谁都是帮助。
	FreeText bool
}

// NoTimeout 放在 Command.Timeout 里表示不设时限。
const NoTimeout time.Duration = -1

// .help 列表和 README 表格里的分组，Groups 是显示顺序。
const (
	GroupSystem  = "运行与维护"
	GroupTools   = "查询与工具"
	GroupAI      = "AI"
	GroupMedia   = "消息与贴纸"
	GroupAdmin   = "群组管理"
	GroupAccount = "账号"
)

// Groups 是分组的显示顺序。
var Groups = []string{GroupSystem, GroupTools, GroupAI, GroupMedia, GroupAdmin, GroupAccount}

// HelpText 是这条命令的详细帮助：有 Help 用 Help，否则用用法加说明。
// 有别名时补一行「简写」，命令自己的帮助已经提到了就不重复。
func (c *Command) HelpText(prefix string) string {
	var text string
	if c.Help != nil {
		text = c.Help(prefix)
	} else {
		usage := prefix + c.Name
		if c.Usage != "" {
			usage += " " + c.Usage
		}
		text = Code(usage) + "\n" + Escape(c.Description)
	}
	var missing []string
	for _, alias := range c.Aliases {
		if !strings.Contains(text, Escape(prefix+alias)+"<") && !strings.Contains(text, Escape(prefix+alias)+" ") {
			missing = append(missing, Code(prefix+alias))
		}
	}
	if len(missing) > 0 {
		text += "\n\n简写：" + strings.Join(missing, " ")
	}
	return text
}

// wantsHelp 判断这次调用是不是只想看帮助：参数只有一个 --help，或者只有一个 help、h
// 而命令不收正文（见 Command.FreeText）。
func (c *Command) wantsHelp(args []string) bool {
	if len(args) != 1 {
		return false
	}
	switch strings.ToLower(args[0]) {
	case "--help":
		return true
	case "help", "h":
		return !c.FreeText
	}
	return false
}

// Job 是账号连上之后启动的后台任务。
type Job func(ctx context.Context, client *bot.Client)

// Registry 保存命令和前缀。
type Registry struct {
	mu sync.RWMutex
	// commands 按名字找命令，Name 和每个 Aliases 各占一项，指向同一个 *Command。
	commands map[string]*Command
	// aliases 把使用者自己起的名字映射到它代表的命令行，参数也包括在内。
	// 由 .alias 修改。
	aliases  map[string]string
	prefixes []string
	jobs     []Job
	logger   *slog.Logger
	// pool 跑命令；tasks 跑代发这类短后台任务；long 跑 .da 这类按小时计的任务。
	// 三者分开，长任务占满了也挡不住命令和代发。
	pool  *scheduler
	tasks *scheduler
	long  *scheduler
	// delayed 跑 After 安排的延时任务；timers 是还没到点的个数，scheduled 不让它无限增长。
	delayed *scheduler
	timers  atomic.Int32
	// tracked 是 Track 记下的、不在这几个池里的业务工作（如定时任务的一次执行）。
	tracked atomic.Int32
	// gate 是维护闸门，见 Quiesce。
	gate *gate
	// life 在 Abort 时取消，所有正在跑和排队的命令都挂在它上面。
	life  context.Context
	abort context.CancelFunc
	// relayed 是账号替别人代发过的命令消息。它们已经按借用规则执行过一次；
	// 万一之后又被当成本人发的消息收到，不能不经检查再执行一遍。
	relayed map[string]time.Time
}

// New 创建一个空的注册表。
func New(prefixes []string, logger *slog.Logger) *Registry {
	if len(prefixes) == 0 {
		prefixes = []string{"."}
	}
	if logger == nil {
		logger = slog.Default()
	}
	life, abort := context.WithCancel(context.Background())
	r := &Registry{commands: map[string]*Command{}, prefixes: append([]string(nil), prefixes...), logger: logger,
		life: life, abort: abort, pool: newScheduler(life, logger, commandWorkers, commandQueue),
		tasks: newScheduler(life, logger, taskWorkers, taskQueue), long: newScheduler(life, logger, LongTasks, LongTasks),
		delayed: newScheduler(life, logger, delayedWorkers, delayedQueue), gate: &gate{}}
	for _, pool := range []*scheduler{r.pool, r.tasks, r.long, r.delayed} {
		pool.gate = r.gate
	}
	return r
}

// Abort 中断所有正在跑和排队的命令，之后收到的命令也不再执行。进程退出（包括重启）前调用：
// 重启常常就是为了掐断卡住的命令，不能让它们拖着不退，也不该等它们跑完。
func (r *Registry) Abort() { r.abort() }

// Lifecycle 是服务的生命周期：比单次更新回调和单个命令活得久，Abort 时取消。
// 看别人消息的监听者和脱离命令的后台任务都要挂在它上面，不能用 WithoutCancel 脱离。
func (r *Registry) Lifecycle() context.Context { return r.life }

// Background 把一件短的后台任务（如代发）交给独立的有界池，不占命令的执行名额。
// 队列满或 ctx 已取消时返回 false 并记日志；接受了的任务算在 Wait 里。
func (r *Registry) Background(ctx context.Context, run func(context.Context)) bool {
	if r.tasks.submit(ctx, run) {
		return true
	}
	r.logger.Warn("task.rejected", slog.String("reason", "queue full, maintenance or shutting down"))
	return false
}

// LongTasks 是同时能跑的长任务个数，也是长任务池的排队上限。
const LongTasks = 4

// LongRunning 把一件按小时计的任务（如 .da）交给长任务池。这个池和 Background 分开：
// 长任务占满了也不耽误代发。调用方应自己把同时进行的个数限制在 LongTasks 以内，
// 否则多出来的会排队等前面的跑完，而不是马上开始。
//
// 接受了的任务结束时调用一次 release，排队期间被取消、run 没有执行时也一样，
// 提交前占下的名额要在这里释放；返回 false 时不调用。release 可以为 nil。
func (r *Registry) LongRunning(ctx context.Context, run func(context.Context), release func()) bool {
	if r.long.admit(ctx, run, release) {
		return true
	}
	r.logger.Warn("task.rejected", slog.String("pool", "long"), slog.String("reason", "queue full, maintenance or shutting down"))
	return false
}

// MaxTimers 是同时等着到点的延时任务上限，免得大量延时删除把计时器堆满内存。
const MaxTimers = 4096

// After 在 delay 之后把 run 交给延时任务池执行（最多 4 个同时跑、256 个排队）。
// 计时期间不占协程和名额；Abort 时还没到点的直接放弃，到点后的 ctx 也随服务退出取消。
// 计时器已满或服务正在退出时返回 false 并记日志；到点时池满了也只记日志、放弃这一次。
func (r *Registry) After(delay time.Duration, run func(context.Context)) bool {
	if r.life.Err() != nil {
		return false
	}
	if r.timers.Add(1) > MaxTimers {
		r.timers.Add(-1)
		r.logger.Warn("task.rejected", slog.String("pool", "delayed"), slog.String("reason", "too many timers"))
		return false
	}
	// stop 要在计时器回调用到之前赋好值，ready 关闭之前回调只能等着。
	ready := make(chan struct{})
	var stop func() bool
	timer := time.AfterFunc(max(delay, 0), func() {
		<-ready
		// 计时器到点了就由这里减计数；下面 Abort 的回调只在拦下了计时器时才减。
		r.timers.Add(-1)
		if !stop() {
			return // 服务正在退出
		}
		if !r.delayed.submit(r.life, run) && r.life.Err() == nil {
			r.logger.Warn("task.rejected", slog.String("pool", "delayed"), slog.String("reason", "queue full or maintenance"))
		}
	})
	stop = context.AfterFunc(r.life, func() {
		if timer.Stop() {
			r.timers.Add(-1)
		}
	})
	close(ready)
	return true
}

// Track 记下一件不在这几个池里跑的业务工作（如 .sum、.acron 的一次定时执行、关键词动作），
// 让 Busy 把它算进去；工作结束时调用返回的 done。维护或退出期间返回 ok=false，
// 调用方应放弃这次工作：服务马上要重启，开始了也会被掐断。
func (r *Registry) Track() (done func(), ok bool) {
	r.gate.mu.RLock()
	defer r.gate.mu.RUnlock()
	if r.gate.closed || r.life.Err() != nil {
		return func() {}, false
	}
	r.tracked.Add(1)
	var once sync.Once
	return func() { once.Do(func() { r.tracked.Add(-1) }) }, true
}

// Quiesce 进入维护状态：先关上准入闸门（之后提交的命令、后台任务、长任务、到点的延时任务
// 和 Track 一律拒绝），再等已经接受的工作全部结束，最多等 timeout。等到了返回 true，闸门
// 保持关着，直到 Resume；等不到或 ctx 取消就重新打开闸门，返回 false。
//
// 闸门关上后工作只减不增，所以这时读到的 Busy 为 0 是可靠的；闸门开着时的 Busy 只能当参考。
func (r *Registry) Quiesce(ctx context.Context, timeout time.Duration) bool {
	r.gate.mu.Lock()
	r.gate.closed = true
	r.gate.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for r.Busy() > 0 {
		select {
		case <-tick.C:
		case <-timer.C:
			r.Resume()
			return false
		case <-ctx.Done():
			r.Resume()
			return false
		}
	}
	if ctx.Err() != nil {
		r.Resume()
		return false
	}
	return true
}

// Resume 重新打开 Quiesce 关上的准入闸门。
func (r *Registry) Resume() {
	r.gate.mu.Lock()
	r.gate.closed = false
	r.gate.mu.Unlock()
}

// Busy 是正在跑和排队的工作总数：命令、后台任务、长任务、到点的延时任务，加上 Track
// 记下的。还没到点的延时任务不算：一条一天后才删的消息不该挡住更新。
//
// 各个池分别读取，闸门开着时不是一致的快照（代发把命令交给命令池的那一刻可能两边都读到 0），
// 只能用来观察和粗略判断；要可靠地确认空闲，用 Quiesce。
func (r *Registry) Busy() int {
	return r.pool.pending() + r.tasks.pending() + r.long.pending() + r.delayed.pending() + int(r.tracked.Load())
}

// Register 添加命令，连同它的别名；名字（包括别名）重复属于编程错误。
func (r *Registry) Register(commands ...*Command) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, command := range commands {
		if command == nil {
			panic("nil command")
		}
		for _, name := range append([]string{command.Name}, command.Aliases...) {
			if !commandName.MatchString(name) {
				panic(fmt.Sprintf("invalid command name %q", name))
			}
			if _, exists := r.commands[name]; exists {
				panic("duplicate command " + name)
			}
			r.commands[name] = command
		}
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

// Commands 列出所有命令（每条一次，不含别名），按名字排序。
func (r *Registry) Commands() []*Command {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := make([]*Command, 0, len(r.commands))
	for name, command := range r.commands {
		if name == command.Name {
			list = append(list, command)
		}
	}
	sort.Slice(list, func(a, b int) bool { return list[a].Name < list[b].Name })
	return list
}

// Lookup 按名字或别名查找命令。
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
		return Route{Prefix: prefix, Command: r.canonical(expanded[0]), Args: args,
			Text: prefix + strings.Join(expanded, " ") + afterTokens(body, length)}, true
	}
	if !commandName.MatchString(parts[0]) {
		return Route{}, false
	}
	return Route{Prefix: prefix, Command: r.canonical(parts[0]), Args: append([]string{}, parts[1:]...), Text: text}, true
}

// canonical 把命令的别名换成它的正式名字，不认得的名字原样返回。
// 路由里只出现正式名字，借用规则、日志就不用再各自认一遍别名。
func (r *Registry) canonical(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if command, ok := r.commands[name]; ok {
		return command.Name
	}
	return name
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

// resolve 把消息文本解析成路由并找到命令。
func (r *Registry) resolve(text string) (Route, *Command, bool) {
	route, ok := r.Parse(text)
	if !ok {
		return Route{}, nil, false
	}
	command, ok := r.Lookup(route.Command)
	return route, command, ok
}

// Result 是派发的结果。
type Result int

const (
	// NotCommand 表示不是命令（或者是代发里不许借出去的命令），没有执行。
	NotCommand Result = iota
	// Queued 表示已经交给命令池，处理函数异步运行；不代表已经执行完。
	Queued
	// Rejected 表示是命令，但命令池已满或服务正在退出，没有执行。
	Rejected
)

// Dispatch 把一条消息交给注册表，返回是否匹配到命令（包括匹配到了但因为排满而没执行的，
// 那种会另记 command.rejected）；处理函数异步运行。要区分有没有真的交出去，用 Submit。
func (r *Registry) Dispatch(ctx context.Context, client *bot.Client, message *bot.Message) bool {
	return r.Submit(ctx, client, message) != NotCommand
}

// Submit 同 Dispatch，但告诉调用方命令有没有真的交给命令池。
func (r *Registry) Submit(ctx context.Context, client *bot.Client, message *bot.Message) Result {
	if r.wasRelayed(message) {
		r.logger.Info("dispatch.relayed_again", slog.String("chat", message.ChatID), slog.Int("message", message.ID))
		return NotCommand
	}
	route, command, ok := r.resolve(message.Text)
	if !ok {
		return NotCommand
	}
	return r.run(ctx, client, message, nil, route, command)
}

// DispatchFor 执行一条账号替别人代发的命令。allowed 决定这条命令能不能借出去，
// 不许就不执行；trigger 是对方原来的那条消息。
func (r *Registry) DispatchFor(ctx context.Context, client *bot.Client, message, trigger *bot.Message, allowed func(Route) bool) Result {
	route, command, ok := r.resolve(message.Text)
	if !ok || !allowed(route) {
		return NotCommand
	}
	r.markRelayed(message)
	return r.run(ctx, client, message, trigger, route, command)
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
func (r *Registry) run(ctx context.Context, client *bot.Client, message, trigger *bot.Message, route Route, command *Command) Result {
	logger := r.logger.With(slog.String("command", route.Command))
	if trigger != nil {
		logger = logger.With(slog.String("for", trigger.ChatID+"#"+strconv.Itoa(trigger.ID)))
	}
	inv := &Invocation{Prefix: route.Prefix, Command: route.Command, Args: route.Args, Text: route.Text,
		Message: message, Client: client, Log: logger, Trigger: trigger}
	if r.life.Err() != nil {
		logger.Info("command.aborted", slog.String("chat", message.ChatID), slog.Int("message", message.ID), slog.String("reason", "shutting down"))
		return Rejected
	}
	accepted := r.pool.submit(ctx, func(live context.Context) {
		runCtx, cancel := live, context.CancelFunc(func() {})
		switch {
		case command.Timeout == NoTimeout:
		case command.Timeout == 0:
			runCtx, cancel = context.WithTimeout(live, 5*time.Minute)
		default:
			runCtx, cancel = context.WithTimeout(live, command.Timeout)
		}
		defer cancel()
		defer func() {
			if recovered := recover(); recovered != nil {
				inv.Log.Error("command.panic", slog.Any("panic", recovered), slog.String("stack", string(debug.Stack())))
				r.reportError(live, inv, "❌ 命令执行时发生内部错误")
			}
		}()
		started := time.Now()
		var err error
		if command.wantsHelp(inv.Args) {
			// 和 MiBox 一样，「命令 --help」只显示帮助、不执行；有的命令根本不看参数，
			// 不拦下来的话 .restart --help 就真的重启了。各命令也就不必自己判断 help。
			err = inv.EditPages(runCtx, HTMLPages(command.HelpText(inv.Prefix), PageLimit))
		} else {
			err = command.Handle(runCtx, inv)
		}
		switch {
		case err == nil:
			inv.Log.Info("command.handled", slog.String("chat", message.ChatID), slog.Int("message", message.ID), slog.Duration("took", time.Since(started)))
		case r.life.Err() != nil:
			// 进程要退出了：不回「执行失败」，连接也正在关，发不出去。
			inv.Log.Info("command.aborted", slog.String("chat", message.ChatID), slog.Int("message", message.ID), slog.Duration("took", time.Since(started)))
		case ctx.Err() != nil:
		case isUserError(err):
			// 用法不对、没有权限、对方不存在这类：不是故障，照原话告诉用户，日志只记一笔。
			text, _ := IsUserError(err)
			attrs := []any{slog.String("chat", message.ChatID), slog.Int("message", message.ID), slog.String("reason", text)}
			level := slog.LevelInfo
			if errors.Unwrap(err) != nil {
				// FailWith 包着的是真的失败（网络、RPC、内部错误），不只是用法不对：记 WARN 并附原始错误。
				level = slog.LevelWarn
				attrs = append(attrs, slog.String("error", err.Error()))
			}
			inv.Log.Log(context.Background(), level, "command.refused", attrs...)
			r.reportError(live, inv, "❌ "+text)
		case errors.Is(err, context.DeadlineExceeded):
			inv.Log.Warn("command.timeout", slog.String("chat", message.ChatID))
			r.reportError(live, inv, "❌ 命令执行超时")
		default:
			inv.Log.Error("command.failed", slog.String("chat", message.ChatID), slog.Int("message", message.ID), slog.String("error", err.Error()))
			r.reportError(live, inv, "❌ 命令执行失败："+Brief(err))
		}
	})
	if !accepted {
		logger.Warn("command.rejected", slog.String("chat", message.ChatID), slog.Int("message", message.ID),
			slog.String("reason", "queue full, maintenance or shutting down"))
		return Rejected
	}
	return Queued
}

// reportError 把失败原因写回命令消息。命令可能已经超时，所以另给 5 秒期限，
// 但仍随服务退出取消：发不出去的提示不能一直占着执行名额。
func (r *Registry) reportError(ctx context.Context, inv *Invocation, text string) {
	if inv.Client == nil || r.life.Err() != nil || ctx.Err() != nil {
		return
	}
	feedback, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = inv.EditText(feedback, text)
}

// Running 是正在执行的命令数（占着执行名额的，不含排队的）。自动更新要重启服务，
// 等它为 0 时再装，免得把正在跑的命令掐断。
func (r *Registry) Running() int { return r.pool.count() }

// Wait 阻塞到所有池都空闲，或者超时为止。Abort 之后不再接受新任务，
// 这时它等的是所有已经接受的命令和任务，包括还在排队的。
func (r *Registry) Wait(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	pools := []*scheduler{r.delayed, r.long, r.tasks, r.pool}
	for {
		for _, pool := range pools {
			if !pool.wait(max(0, time.Until(deadline))) {
				return false
			}
		}
		// 代发会提交命令，命令又会启动后台任务：逐个等完之后，前面的池可能又忙了，
		// 要同时锁住所有池确认都空着才算完。
		for _, pool := range pools {
			pool.mu.Lock()
		}
		idle := true
		for _, pool := range pools {
			idle = idle && pool.workers == 0
		}
		for _, pool := range pools {
			pool.mu.Unlock()
		}
		if idle {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}
