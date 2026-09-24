// Package app 把整个进程组装起来：读取账号、转换会话、加锁、连接 gotd，
// 再把更新交给命令注册表。
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	gotdlog "github.com/gotd/log"
	"github.com/gotd/log/logslog"
	gotdsession "github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"golang.org/x/net/proxy"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/config"
	"github.com/MiCat-S/mibot-lite/internal/logtail"
	"github.com/MiCat-S/mibot-lite/internal/session"
	"github.com/MiCat-S/mibot-lite/internal/tgstate"
)

// Options 是一次运行的配置。
type Options struct {
	Root    string
	Version string
	Logger  *slog.Logger
	Debug   bool
	// Register 在注册表建好之后添加命令。
	Register func(app *App)
	// Logs 保存最近的一段日志，供 .log 显示。没有它程序照样能跑，
	// 只是 .log 会说明当前没有保留日志。
	Logs *logtail.Ring
	// Level 是运行中可调的日志级别。把它交进来，操作者就能在聊天里调高级别；
	// 不然就得改服务单元再重启，而重启恰好会把要查的那一刻弄丢。
	Level *slog.LevelVar
	// ReadOnly 表示除了单实例锁，其他都照常准备。
	//
	// 这把锁的意思是「一个账号只能由一个进程服务」，而只读检查什么也不服务。
	// 以前照样加锁，结果只要服务在运行，--check 就会失败；自动更新偏偏就是
	// 在这时候运行它，于是好好的新版本被当成读不了而丢弃。
	ReadOnly bool
	// AfterReady 在账号连上、命令开始服务之后运行，它一返回 Run 就结束。
	// --verify 靠它操作线上账号，不必另开一条连接路径。
	AfterReady func(ctx context.Context, a *App, client *bot.Client) error
}

// App 是组装好的一个进程。
type App struct {
	Root     string
	Version  string
	Logger   *slog.Logger
	Config   *config.Config
	Env      config.Env
	Registry *command.Registry
	Started  time.Time
	BootID   string
	Logs     *logtail.Ring
	Level    *slog.LevelVar

	client *telegram.Client
	gaps   *updates.Manager
	peers  *bot.PeerCache
	state  *tgstate.State
	lock   *os.File
	bot    atomic.Pointer[bot.Client]
	// options 留着是为了让 Run 能拿到 AfterReady。
	options    Options
	hookResult atomic.Pointer[error]
	// foreign 是看别人消息的监听者（.sure、.sudo）。只在注册阶段添加，之后只读。
	foreign []ForeignListener
}

// sleepFor 等待一段时间，ctx 结束时提前返回。
func sleepFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// SessionFile 是 gotd 的会话文件，和 MiBox 的 Go 宿主共用。
const SessionFile = "gotd-session.json"

// ErrRunning 表示部署目录已被另一个进程占用。
var ErrRunning = errors.New("another mibot-lite instance already runs on this directory")

// LockRoot 给部署目录加单实例锁。
//
// 服务时加锁，是为了不让两个进程同时替一个账号应答。恢复备份也要加锁，
// 理由正好反过来：在运行中的服务底下改写 config.json，服务手里的会话
// 就和文件里记的对不上了。
func LockRoot(root string) (*os.File, error) {
	lock, err := os.OpenFile(filepath.Join(root, "mibot-lite.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, ErrRunning
	}
	return lock, nil
}

// DataDir 是命令存放 JSON 文件的目录。
func (a *App) DataDir() string { return filepath.Join(a.Root, "data") }

// Prepare 读取账号、转换会话并加锁，不做任何网络 I/O。
func Prepare(ctx context.Context, options Options) (*App, error) {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	root, err := filepath.Abs(options.Root)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Read(root)
	if err != nil {
		return nil, err
	}
	env := config.ReadEnv(root, os.Environ())

	storage := &gotdsession.FileStorage{Path: filepath.Join(root, SessionFile)}
	if _, err := session.Import(ctx, storage, cfg.Session, false); err != nil {
		return nil, fmt.Errorf("convert session: %w", err)
	}

	var lock *os.File
	if !options.ReadOnly {
		if lock, err = LockRoot(root); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o700); err != nil {
		if lock != nil {
			lock.Close()
		}
		return nil, err
	}

	app := &App{Root: root, Version: options.Version, Logger: logger, Config: cfg, Env: env,
		Registry: command.New(env.Prefixes(), logger), Started: time.Now(), BootID: strconv.FormatInt(time.Now().UnixNano(), 36),
		Logs: options.Logs, Level: options.Level,
		peers: bot.NewPeerCache(), lock: lock, options: options}

	state, err := tgstate.Open(filepath.Join(root, "updates.json"))
	if err != nil {
		logger.Warn("updates.state_unavailable", slog.String("error", err.Error()))
	} else {
		app.state = state
		app.peers.SetDurable(state)
	}

	protocol := protocolLogger(logger, options.Debug)
	gapsConfig := updates.Config{
		Handler: loggingHandler{next: app.dispatcher(), logger: logger},
		Logger:  protocol,
		OnChannelTooLong: func(channelID int64) {
			logger.Warn("updates.channel_too_long", slog.Int64("channel", channelID))
		},
		OnTooLong: func() { logger.Warn("updates.too_long") },
	}
	if state != nil {
		gapsConfig.Storage = state
		gapsConfig.AccessHasher = state
		gapsConfig.UserAccessHasher = state
	}
	app.gaps = updates.New(gapsConfig)

	clientOptions := telegram.Options{
		SessionStorage: storage,
		Device:         telegram.DeviceConfig{DeviceModel: cfg.DeviceModel},
		Logger:         protocol,
		UpdateHandler:  app.gaps,
		// 补上 MiBox 所用的 teleproto 对每个请求都做、gotd 不做的事：60 秒以内的限流和
		// 服务端内部错误自动重试（最多 5 次），每个应答里的用户和群都记进缓存。
		// 从 MiBox 移植来的命令都默认有这层保护。发出去的消息里的 IP 按 .privacy 的设置打码，
		// 同 MiBox v2。
		Middlewares: []telegram.Middleware{
			// 最外层：一次发送只打一遍码，重试时发的是同一份打过码的请求。
			bot.IPRedactor{},
			bot.Retrier{Logger: app.Logger, MaxWait: 60 * time.Second, Attempts: 5},
			bot.EntityRecorder{Peers: app.peers},
		},
	}
	if cfg.Proxy != nil {
		var auth *proxy.Auth
		if cfg.Proxy.Username != "" || cfg.Proxy.Password != "" {
			auth = &proxy.Auth{User: cfg.Proxy.Username, Password: cfg.Proxy.Password}
		}
		dialer, err := proxy.SOCKS5("tcp", net.JoinHostPort(cfg.Proxy.IP, strconv.Itoa(cfg.Proxy.Port)), auth, &net.Dialer{Timeout: 10 * time.Second})
		if err != nil {
			lock.Close()
			return nil, fmt.Errorf("proxy: %w", err)
		}
		clientOptions.Resolver = dcs.Plain(dcs.PlainOptions{Dial: dialer.(proxy.ContextDialer).DialContext})
	}
	app.client = telegram.NewClient(cfg.APIID, cfg.APIHash, clientOptions)
	if options.Register != nil {
		options.Register(app)
	}
	return app, nil
}

// protocolLogger 把 slog 适配给 gotd。gotd 的协议日志在 info 和 debug
// 级别非常啰嗦，所以不加 --verbose 时只放行警告和错误，加了才全部放行。
func protocolLogger(logger *slog.Logger, debug bool) gotdlog.Logger {
	floor := slog.LevelWarn
	if debug {
		floor = slog.LevelDebug
	}
	return logslog.New(slog.New(levelFilter{handler: logger.Handler(), floor: floor}))
}

type levelFilter struct {
	handler slog.Handler
	floor   slog.Level
}

func (f levelFilter) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= f.floor && f.handler.Enabled(ctx, level)
}
func (f levelFilter) Handle(ctx context.Context, record slog.Record) error {
	return f.handler.Handle(ctx, record)
}
func (f levelFilter) WithAttrs(attrs []slog.Attr) slog.Handler {
	return levelFilter{handler: f.handler.WithAttrs(attrs), floor: f.floor}
}
func (f levelFilter) WithGroup(name string) slog.Handler {
	return levelFilter{handler: f.handler.WithGroup(name), floor: f.floor}
}

// loggingHandler 赶在本程序自己的任何过滤之前，记下服务器实际推送了什么。
//
// 之所以需要它，是因为「命令什么也没做」和「更新根本没到」表现完全一样，
// 都是没有动静，而把 gotd 读得再细也分辨不出是哪一种。日志量不大：
// 繁忙的账号每分钟也就推送几条。
type loggingHandler struct {
	next   telegram.UpdateHandler
	logger *slog.Logger
}

func (h loggingHandler) Handle(ctx context.Context, updates tg.UpdatesClass) error {
	switch value := updates.(type) {
	case *tg.Updates:
		h.logger.Debug("update.batch", slog.String("kind", "Updates"), slog.Any("types", updateNames(value.Updates)))
	case *tg.UpdatesCombined:
		h.logger.Debug("update.batch", slog.String("kind", "UpdatesCombined"), slog.Any("types", updateNames(value.Updates)))
	case *tg.UpdateShort:
		h.logger.Debug("update.batch", slog.String("kind", "UpdateShort"), slog.Any("types", updateNames([]tg.UpdateClass{value.Update})))
	default:
		h.logger.Debug("update.batch", slog.String("kind", fmt.Sprintf("%T", updates)))
	}
	return h.next.Handle(ctx, updates)
}

// updateNames 列出一批更新的类型名，带消息的更新附上所在对话。
func updateNames(list []tg.UpdateClass) []string {
	names := make([]string, 0, len(list))
	for _, item := range list {
		name := fmt.Sprintf("%T", item)
		if index := strings.LastIndexByte(name, '.'); index >= 0 {
			name = name[index+1:]
		}
		switch value := item.(type) {
		case *tg.UpdateNewMessage:
			name += "(" + messageChat(value.Message) + ")"
		case *tg.UpdateNewChannelMessage:
			name += "(" + messageChat(value.Message) + ")"
		case *tg.UpdateEditMessage:
			name += "(" + messageChat(value.Message) + ")"
		}
		names = append(names, name)
	}
	return names
}

func messageChat(message tg.MessageClass) string {
	plain, ok := message.(*tg.Message)
	if !ok {
		return "?"
	}
	return bot.PeerID(plain.PeerID)
}

func (a *App) dispatcher() tg.UpdateDispatcher {
	dispatcher := tg.NewUpdateDispatcher()
	dispatcher.OnNewMessage(func(ctx context.Context, entities tg.Entities, update *tg.UpdateNewMessage) error {
		a.handle(ctx, entities, update.Message, false)
		return nil
	})
	dispatcher.OnNewChannelMessage(func(ctx context.Context, entities tg.Entities, update *tg.UpdateNewChannelMessage) error {
		a.handle(ctx, entities, update.Message, false)
		return nil
	})
	dispatcher.OnEditMessage(func(ctx context.Context, entities tg.Entities, update *tg.UpdateEditMessage) error {
		a.handle(ctx, entities, update.Message, true)
		return nil
	})
	dispatcher.OnEditChannelMessage(func(ctx context.Context, entities tg.Entities, update *tg.UpdateEditChannelMessage) error {
		a.handle(ctx, entities, update.Message, true)
		return nil
	})
	return dispatcher
}

// handle 把一条协议消息规整好，交给注册表。只有本账号自己新发的消息
// 才算命令：编辑证明不了是谁在键盘前，而且 bot 自己编辑结果时也是
// 这个样子。
//
// 每条丢弃消息的路径都会留下记录。否则「输入了命令却没反应」和「更新
// 根本没到」无从区分，而两者的原因完全不同。排查这类问题时，大部分
// 时间都花在分辨是哪一种上。
func (a *App) handle(ctx context.Context, entities tg.Entities, message tg.MessageClass, edited bool) {
	plain, ok := message.(*tg.Message)
	if !ok {
		return
	}
	a.peers.Remember(entities)
	client := a.bot.Load()
	if client == nil {
		return
	}
	// 能不能解析成命令，决定了丢弃时记录的级别：普通消息经过只是
	// debug 噪音；操作者输入了命令却没等到回应，就该记在他们真正会看的
	// 级别上。
	_, looksLikeCommand := a.Registry.Parse(plain.Message)
	envelope, converted := bot.Envelope(plain, client.SelfID(), edited, a.peers)
	// 这条是不是本账号自己写的，这才是命令关卡真正要问的。只看 Out 标志，
	// 在收藏夹里会答错：和自己的对话没有方向，Telegram 不设这个标志，
	// 结果在那里输入的每条命令都被当成别人的消息丢掉了。
	mine := plain.Out || (converted && envelope.SenderID() == client.SelfID())
	if mine {
		a.Logger.Debug("update.outgoing", slog.String("chat", bot.PeerID(plain.PeerID)),
			slog.Int("message", plain.ID), slog.Bool("command", looksLikeCommand))
	}
	drop := func(reason string) {
		if looksLikeCommand && mine {
			a.Logger.Info("dispatch.dropped", slog.String("reason", reason),
				slog.Int("message", plain.ID), slog.String("text", truncate(plain.Message, 40)))
			return
		}
		a.Logger.Debug("dispatch.skipped", slog.String("reason", reason), slog.Int("message", plain.ID))
	}
	switch {
	case edited:
		drop("edited")
		return
	case !converted:
		drop("unaddressable peer")
		return
	case !mine:
		// 别人的消息：转发的、编辑过的不算，其余交给借用规则看一眼。
		if !envelope.Edited && !envelope.Forward && a.OfferForeign(context.WithoutCancel(ctx), client, envelope) {
			return
		}
		drop("not written by this account")
		return
	case envelope.Edited:
		drop("carries an edit date")
		return
	case envelope.Forward:
		drop("forwarded")
		return
	}
	if !a.Registry.Dispatch(context.WithoutCancel(ctx), client, envelope) {
		drop("no command matched")
	}
}

// ForeignListener 看一条别人发的消息，处理了就返回 true。
type ForeignListener func(ctx context.Context, client *bot.Client, message *bot.Message) bool

// OnForeign 登记一个看别人消息的监听者。只能在注册命令时调用；
// 按登记顺序依次询问，第一个处理了的为准。
func (a *App) OnForeign(listener ForeignListener) {
	a.foreign = append(a.foreign, listener)
}

// OfferForeign 把一条别人的消息依次交给监听者，返回有没有谁处理了。
// 监听者要自己判断得快：这里还在处理更新的路上，耗时的活得放到后台。
func (a *App) OfferForeign(ctx context.Context, client *bot.Client, message *bot.Message) bool {
	for _, listener := range a.foreign {
		if listener(ctx, client, message) {
			return true
		}
	}
	return false
}

// truncate 截短文本，用在日志行里。
func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// Bot 返回已连接的客户端；授权完成之前返回 nil。
func (a *App) Bot() *bot.Client { return a.bot.Load() }

// DispatchMessage 把一条协议消息交给命令注册表，做法和更新路径一样。
//
// 它是为 --verify 准备的，--verify 没有别的办法够到分发器。本进程发出的
// 消息不会再推送回来，服务器改在 RPC 结果里报告；对纯文本来说，这个结果
// 是 updateShortSentMessage，只带一个 id 和一个 pts，既没有 peer 也没有
// 文本。所以校验程序把消息完整读回来，再交到这里。
func (a *App) DispatchMessage(ctx context.Context, message *tg.Message) bool {
	client := a.bot.Load()
	if client == nil {
		return false
	}
	envelope, ok := bot.Envelope(message, client.SelfID(), false, a.peers)
	if !ok {
		return false
	}
	return a.Registry.Dispatch(ctx, client, envelope)
}

// Close 释放锁并把状态刷写到磁盘。
func (a *App) Close() error {
	if a.state != nil {
		_ = a.state.Close()
	}
	if a.lock != nil {
		return a.lock.Close()
	}
	return nil
}

// Run 连接、认证、启动后台任务，然后持续处理更新，直到 ctx 结束。
func (a *App) Run(ctx context.Context) error {
	return a.client.Run(ctx, func(ctx context.Context) error {
		status, err := a.client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("check authorization: %w", err)
		}
		if !status.Authorized {
			return errors.New("session is not authorized; run --login first")
		}
		self, err := a.client.Self(ctx)
		if err != nil {
			return fmt.Errorf("resolve account: %w", err)
		}
		a.peers.SetSelf(self.ID)
		a.peers.RememberUsers([]tg.UserClass{self})
		client := bot.New(a.client, a.peers, self, a.Logger)
		defer client.CloseDataCentres()
		a.bot.Store(client)
		a.Logger.Info("runtime.ready", slog.Int64("account", self.ID), slog.Int("commands", len(a.Registry.Commands())), slog.String("version", a.Version))
		for _, job := range a.Registry.Jobs() {
			go job(ctx, client)
		}
		if a.options.AfterReady != nil {
			// 更新引擎在运行，命令才会被分发，所以钩子和它并行运行，
			// 钩子结束时再把它停掉。
			serving, stop := context.WithCancel(ctx)
			defer stop()
			hookErr := make(chan error, 1)
			go func() {
				// 发第一条消息之前，给更新引擎一点时间加载状态。
				if err := sleepFor(serving, 2*time.Second); err != nil {
					hookErr <- nil
					return
				}
				hookErr <- a.options.AfterReady(serving, a, client)
			}()
			go func() {
				err := <-hookErr
				a.hookResult.Store(&err)
				stop()
			}()
			err = a.gaps.Run(serving, a.client.API(), self.ID, updates.AuthOptions{IsBot: self.Bot})
			a.Registry.Wait(10 * time.Second)
			if result := a.hookResult.Load(); result != nil {
				return *result
			}
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		err = a.gaps.Run(ctx, a.client.API(), self.ID, updates.AuthOptions{IsBot: self.Bot})
		a.Registry.Wait(10 * time.Second)
		return err
	})
}
