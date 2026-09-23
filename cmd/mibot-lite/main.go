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
	"time"
	_ "time/tzdata"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/backup"
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
		force       = flag.Bool("force", false, "with --login or --restore, replace an existing account")
		restoreFrom = flag.String("restore", "", "unpack a backup made by .bf or --backup into --root; the service on that directory must be stopped")
		backupTo    = flag.String("backup", "", "write the same backup .bf sends, to a file, without connecting")
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
	if *backupTo != "" {
		archive, names, err := backup.Create(*root, displayVersion(), time.Now())
		if err == nil {
			err = os.WriteFile(*backupTo, archive, 0o600)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "mibot-lite --backup:", err)
			os.Exit(1)
		}
		fmt.Printf("Backed up %d files to %s. It holds the login session: keep it private.\n", len(names), *backupTo)
		return
	}
	if *restoreFrom != "" {
		if err := restore(*restoreFrom, *root, *force); err != nil {
			fmt.Fprintln(os.Stderr, "mibot-lite --restore:", err)
			os.Exit(1)
		}
		return
	}
	if *signIn {
		if err := login.Run(ctx, login.Options{Root: *root, APIID: *apiID, APIHash: *apiHash, Force: *force}); err != nil {
			fmt.Fprintln(os.Stderr, "mibot-lite --login:", err)
			os.Exit(1)
		}
		return
	}
	if !*serve && !*check && !*check2 {
		fmt.Fprintln(os.Stderr, "usage: mibot-lite --serve | --check | --verify | --login [--force] | --backup FILE | --restore FILE [--force] | --import-mibox DIR [--root DIR]")
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

// restore unpacks a backup into root.
//
// It takes the same lock serving does. A running service holds a session
// in memory and writes its state back as it goes; rewriting config.json
// underneath it would leave the two disagreeing about which account this
// directory is.
func restore(file, root string, overwrite bool) error {
	archive, err := os.Open(file)
	if err != nil {
		return err
	}
	defer archive.Close()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	lock, err := app.LockRoot(root)
	if errors.Is(err, app.ErrRunning) {
		return errors.New("the service is running on " + root + "; stop it first (systemctl stop mibot-lite)")
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	names, err := backup.Restore(archive, root, overwrite)
	if errors.Is(err, backup.ErrExists) {
		return errors.New(root + " already has an account; add --force to replace it with the backup")
	}
	if err != nil {
		return err
	}
	fmt.Printf("Restored %d files into %s:\n", len(names), root)
	for _, name := range names {
		fmt.Println("  " + name)
	}
	return nil
}
