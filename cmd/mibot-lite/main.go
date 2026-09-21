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
	"github.com/MiCat-S/mibot-lite/internal/commands"
	"github.com/MiCat-S/mibot-lite/internal/login"
	"github.com/MiCat-S/mibot-lite/internal/sysinfo"
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
		if !*serve && !*check {
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
	if !*serve && !*check {
		fmt.Fprintln(os.Stderr, "usage: mibot-lite --serve | --check | --login [--force] | --import-mibox DIR [--root DIR]")
		os.Exit(2)
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	current, err := app.Prepare(ctx, app.Options{Root: *root, Version: version, Logger: logger, Debug: *verbose, Register: commands.RegisterAll})
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
}

func displayVersion() string {
	if version == "" {
		return "dev"
	}
	return version
}
