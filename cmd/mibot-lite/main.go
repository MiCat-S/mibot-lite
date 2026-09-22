// Command mibot-lite is a small Telegram userbot: one static Go binary,
// commands written in Go, JSON files for state. It reads the same
// config.json MiBox writes, so an existing account carries over.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	_ "time/tzdata"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/commands"
	"github.com/MiCat-S/mibot-lite/internal/config"
	"github.com/MiCat-S/mibot-lite/internal/login"
	"github.com/MiCat-S/mibot-lite/internal/logtail"
	"github.com/MiCat-S/mibot-lite/internal/sysinfo"
	"github.com/MiCat-S/mibot-lite/internal/verify"
)

// version is injected at build time (scripts/build.sh).
var version = ""

func main() {
	var (
		root        = flag.String("root", ".", "deployment directory holding config.json")
		serve       = flag.Bool("serve", false, "connect and serve commands")
		check       = flag.Bool("check", false, "validate the account and session without connecting")
		signIn      = flag.Bool("login", false, "sign a Telegram account in and write config.json under --root")
		force       = flag.Bool("force", false, "with --login, replace an existing session")
		apiID       = flag.Int("api-id", 0, "with --login, the api_id from my.telegram.org")
		apiHash     = flag.String("api-hash", "", "with --login, the api_hash from my.telegram.org")
		importMibox = flag.String("import-mibox", "", "copy the plugin data files of a MiBox deployment directory into --root/data")
		check2      = flag.Bool("verify", false, "connect and run every read-only command against the live account, in Saved Messages")
		verbose     = flag.Bool("verbose", false, "log at debug level, including the protocol trace")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(displayVersion())
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *importMibox != "" {
		if err := commands.ImportMiBox(*importMibox, filepath.Join(*root, "data"), os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "mibot-lite --import-mibox:", err)
			os.Exit(1)
		}
		if !*serve && !*check && !*check2 {
			return
		}
	}
	if *signIn {
		if err := login.Run(ctx, login.Options{Root: *root, APIID: *apiID, APIHash: *apiHash, Force: *force}); err != nil {
			fmt.Fprintln(os.Stderr, "mibot-lite --login:", err)
			os.Exit(1)
		}
		return
	}
	if !*serve && !*check && !*check2 {
		fmt.Fprintln(os.Stderr, "usage: mibot-lite --serve | --check | --verify | --login [--force] | --import-mibox DIR [--root DIR]")
		os.Exit(2)
	}

	// A variable rather than a constant level: .log can raise it while
	// the service runs, which is the difference between diagnosing a
	// quiet failure and restarting the account to watch for it.
	level := new(slog.LevelVar)
	if *verbose {
		level.Set(slog.LevelDebug)
	}
	logs := logtail.New()
	logger := slog.New(logtail.Wrap(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}), logs))

	// A check reads; it does not serve. Asking for the single-instance
	// lock would make it fail precisely when the service is up, which is
	// when the self-updater runs it against a freshly downloaded build.
	options := app.Options{Root: *root, Version: version, Logger: logger, Debug: *verbose,
		Logs: logs, Level: level, Register: commands.RegisterAll, ReadOnly: *check}
	failures := 0
	if *check2 {
		options.AfterReady = func(ctx context.Context, a *app.App, client *bot.Client) error {
			count, err := verify.Run(ctx, client, prefixOf(options), os.Stdout, verify.Cases, a.DispatchMessage)
			failures = count
			return err
		}
	}
	current, err := app.Prepare(ctx, options)
	if err != nil {
		logger.Error("startup.failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer current.Close()

	if *check {
		memory := sysinfo.Read()
		logger.Info("check.ok", slog.String("version", displayVersion()), slog.Int("commands", len(current.Registry.Commands())),
			slog.Float64("rss_mb", sysinfo.Megabytes(memory.RSS)), slog.Float64("heap_mb", sysinfo.Megabytes(memory.HeapAlloc)))
		return
	}
	if err := current.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("runtime.failed", slog.String("error", err.Error()))
		current.Close()
		os.Exit(1)
	}
	if failures > 0 {
		current.Close()
		os.Exit(1)
	}
}

// prefixOf reads the command prefix a deployment uses, for --verify.
func prefixOf(options app.Options) string {
	return config.ReadEnv(options.Root, os.Environ()).Prefixes()[0]
}

func displayVersion() string {
	if version == "" {
		return "dev"
	}
	return version
}
