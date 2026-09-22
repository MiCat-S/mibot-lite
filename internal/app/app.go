// Package app assembles the process: it reads the account, converts its
// session, takes the lock, connects gotd, and routes updates to the
// command registry.
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
	"github.com/MiCat-S/mibot-lite/internal/session"
	"github.com/MiCat-S/mibot-lite/internal/tgstate"
)

// Options configure a run.
type Options struct {
	Root    string
	Version string
	Logger  *slog.Logger
	Debug   bool
	// Register adds the commands once the registry exists.
	Register func(app *App)
	// AfterReady runs once the account is connected and the commands are
	// serving. When it returns, Run stops. It is how --verify drives the
	// live account without a second connection path.
	AfterReady func(ctx context.Context, a *App, client *bot.Client) error
}

// App is one assembled process.
type App struct {
	Root     string
	Version  string
	Logger   *slog.Logger
	Config   *config.Config
	Env      config.Env
	Registry *command.Registry
	Started  time.Time
	BootID   string

	client *telegram.Client
	gaps   *updates.Manager
	peers  *bot.PeerCache
	state  *tgstate.State
	lock   *os.File
	bot    atomic.Pointer[bot.Client]
	// options is kept so Run can reach AfterReady.
	options    Options
	hookResult atomic.Pointer[error]
}

// sleepFor waits, or returns when ctx ends.
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

// SessionFile is gotd's session file, shared with MiBox's Go host.
const SessionFile = "gotd-session.json"

// DataDir is where the commands keep their JSON files.
func (a *App) DataDir() string { return filepath.Join(a.Root, "data") }

// Prepare reads the account, converts its session and takes the lock. It
// performs no network I/O.
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

	lock, err := os.OpenFile(filepath.Join(root, "mibot-lite.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("another mibot-lite instance already runs on this directory")
	}
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o700); err != nil {
		lock.Close()
		return nil, err
	}

	app := &App{Root: root, Version: options.Version, Logger: logger, Config: cfg, Env: env,
		Registry: command.New(env.Prefixes(), logger), Started: time.Now(), BootID: strconv.FormatInt(time.Now().UnixNano(), 36),
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
		Handler: app.dispatcher(),
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

// protocolLogger adapts slog for gotd. Below debug the protocol trace is
// too chatty, so only warnings and errors pass unless --verbose.
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

// handle normalises one protocol message and offers it to the registry.
// Only the account's own fresh messages are commands: an edit does not
// prove who is at the keyboard, and it is also what the bot's own result
// edits look like.
func (a *App) handle(ctx context.Context, entities tg.Entities, message tg.MessageClass, edited bool) {
	plain, ok := message.(*tg.Message)
	if !ok {
		return
	}
	a.peers.Remember(entities)
	client := a.bot.Load()
	if client == nil || edited || !plain.Out {
		return
	}
	envelope, ok := bot.Envelope(plain, client.SelfID(), edited, a.peers)
	if !ok || envelope.Edited || envelope.Forward {
		return
	}
	a.Registry.Dispatch(context.WithoutCancel(ctx), client, envelope)
}

// Bot returns the connected client, or nil before authorization.
func (a *App) Bot() *bot.Client { return a.bot.Load() }

// DispatchMessage offers a protocol message to the command registry, the
// same way the update path does.
//
// It exists for --verify, which cannot reach the dispatcher any other way.
// A message this process sends is never pushed back to it — the server
// reports it in the RPC result instead, and for plain text that result is
// an updateShortSentMessage carrying only an id and a pts, with no peer
// and no text. So the verifier reads the message back in full and hands it
// here.
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

// Close releases the lock and flushes state.
func (a *App) Close() error {
	if a.state != nil {
		_ = a.state.Close()
	}
	if a.lock != nil {
		return a.lock.Close()
	}
	return nil
}

// Run connects, authenticates, starts the jobs and serves updates until
// ctx is done.
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
			// The update engine has to be running for commands to be
			// dispatched at all, so the hook runs beside it and stops it
			// when it finishes.
			serving, stop := context.WithCancel(ctx)
			defer stop()
			hookErr := make(chan error, 1)
			go func() {
				// Give the update engine a moment to load its state
				// before the first message is sent.
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
