// Command mibot-lite 是一个小巧的 Telegram userbot：单个静态 Go 二进制，
// 命令用 Go 写，状态存在 JSON 文件里。它读的 config.json 和 MiBox 写的
// 是同一个文件，所以已有的账号可以直接沿用。
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

// version 在构建时注入（scripts/build.sh）。
var version = ""

// flags 是命令行参数。
type flags struct {
	root        string
	serve       bool
	check       bool
	verifyLive  bool
	signIn      bool
	force       bool
	restoreFrom string
	backupTo    string
	apiID       int
	apiHash     string
	importMibox string
	verbose     bool
	showVersion bool
}

const usageText = `用法：mibot-lite 模式 [--root 部署目录]

模式（选一个）：
  --serve                 连上 Telegram 提供服务（systemd 就是这样启动的）
  --check                 只检查部署目录能不能读，不连网、不改文件
  --verify                连上真实账号，在收藏夹里跑一组只读命令做自检（要先停掉服务）
  --login [--force]       登录账号，写 config.json；--api-id、--api-hash 可以省掉前两个提问
  --backup 文件           把 .bf 会发的那份备份写到文件，不连网
  --restore 文件 [--force] 用备份恢复部署目录（服务要先停掉；--force 覆盖已有账号）
  --import-mibox 目录     把 MiBox 部署里的命令配置搬进 --root/data，并把 TB_PREFIX 写进 .env；
                          可以和 --serve、--check、--verify 一起用
  --version               显示版本

通用：
  --root 部署目录          默认是当前目录 .
  --verbose               日志用 debug 级别，包括协议细节

参数：
`

func parseFlags() flags {
	var f flags
	flag.StringVar(&f.root, "root", ".", "部署目录，里面有 config.json")
	flag.BoolVar(&f.serve, "serve", false, "连上 Telegram 提供服务")
	flag.BoolVar(&f.check, "check", false, "只检查部署目录能不能读，不连网、不改文件")
	flag.BoolVar(&f.verifyLive, "verify", false, "连上真实账号，在收藏夹里跑一组只读命令做自检")
	flag.BoolVar(&f.signIn, "login", false, "登录 Telegram 账号，把 config.json 写进 --root")
	flag.BoolVar(&f.force, "force", false, "和 --login 或 --restore 一起用：替换已有的账号")
	flag.StringVar(&f.restoreFrom, "restore", "", "把 .bf 或 --backup 做的备份解到 --root；那个目录上的服务要先停掉")
	flag.StringVar(&f.backupTo, "backup", "", "把 .bf 会发的那份备份写到这个文件，不连网")
	flag.IntVar(&f.apiID, "api-id", 0, "和 --login 一起用：my.telegram.org 上的 api_id")
	flag.StringVar(&f.apiHash, "api-hash", "", "和 --login 一起用：my.telegram.org 上的 api_hash")
	flag.StringVar(&f.importMibox, "import-mibox", "", "把这个 MiBox 部署目录里的命令配置搬进 --root/data")
	flag.BoolVar(&f.verbose, "verbose", false, "日志用 debug 级别，包括协议细节")
	flag.BoolVar(&f.showVersion, "version", false, "显示版本后退出")
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), usageText)
		flag.PrintDefaults()
	}
	flag.Parse()
	return f
}

func main() { os.Exit(run(parseFlags())) }

// run 按参数选一种模式执行，返回退出码。
func run(f flags) int {
	if f.showVersion {
		fmt.Println(displayVersion())
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	connects := f.serve || f.check || f.verifyLive
	if f.importMibox != "" {
		if err := commands.ImportMiBox(f.importMibox, filepath.Join(f.root, "data"), os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "mibot-lite --import-mibox:", err)
			return 1
		}
		if !connects {
			return 0
		}
	}
	switch {
	case f.backupTo != "":
		return runBackup(f)
	case f.restoreFrom != "":
		return runRestore(f)
	case f.signIn:
		if err := login.Run(ctx, login.Options{Root: f.root, APIID: f.apiID, APIHash: f.apiHash, Force: f.force}); err != nil {
			fmt.Fprintln(os.Stderr, "mibot-lite --login:", err)
			return 1
		}
		return 0
	case !connects:
		flag.Usage()
		return 2
	}
	return runService(ctx, f)
}

// runBackup 把备份写到文件，不连网。
func runBackup(f flags) int {
	archive, names, err := backup.Create(f.root, displayVersion(), time.Now())
	if err == nil {
		err = os.WriteFile(f.backupTo, archive, 0o600)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mibot-lite --backup:", err)
		return 1
	}
	fmt.Printf("已把 %d 个文件备份到 %s。里面有登录会话，不要给别人。\n", len(names), f.backupTo)
	return 0
}

// runRestore 用备份恢复部署目录。
func runRestore(f flags) int {
	if err := restore(f.restoreFrom, f.root, f.force); err != nil {
		fmt.Fprintln(os.Stderr, "mibot-lite --restore:", err)
		return 1
	}
	return 0
}

// runService 处理 --serve、--check、--verify：组装应用，检查就返回，否则连上去运行。
func runService(ctx context.Context, f flags) int {
	// 日志级别用变量而不是常量：服务运行中 .log 可以把它调高。
	// 有了这一点，排查不声不响的故障时，就不必重启账号再守着它复现。
	level := new(slog.LevelVar)
	if f.verbose {
		level.Set(slog.LevelDebug)
	}
	logs := logtail.New()
	logger := slog.New(logtail.Wrap(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}), logs))

	// --check 只读，不提供服务。要是它也去拿单实例锁，就恰好会在服务
	// 运行时失败，而自更新正是在这个时候拿它检查刚下载的新版本。
	options := app.Options{Root: f.root, Version: version, Logger: logger, Debug: f.verbose,
		Logs: logs, Level: level, Register: commands.RegisterAll, ReadOnly: f.check}
	failures := 0
	if f.verifyLive {
		options.AfterReady = func(ctx context.Context, a *app.App, client *bot.Client) error {
			count, err := verify.Run(ctx, client, prefixOf(options), os.Stdout, verify.Cases, a.DispatchMessage)
			failures = count
			return err
		}
	}
	current, err := app.Prepare(ctx, options)
	if err != nil {
		logger.Error("startup.failed", slog.String("error", err.Error()))
		return 1
	}
	defer current.Close()

	if f.check {
		memory := sysinfo.Read()
		logger.Info("check.ok", slog.String("version", displayVersion()), slog.Int("commands", len(current.Registry.Commands())),
			slog.Float64("rss_mb", sysinfo.Megabytes(memory.RSS)), slog.Float64("heap_mb", sysinfo.Megabytes(memory.HeapAlloc)))
		return 0
	}
	if err := current.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("runtime.failed", slog.String("error", err.Error()))
		return 1
	}
	if failures > 0 {
		return 1
	}
	return 0
}

// prefixOf 读出部署所用的命令前缀，供 --verify 使用。
func prefixOf(options app.Options) string {
	return config.ReadEnv(options.Root, os.Environ()).Prefixes()[0]
}

func displayVersion() string {
	if version == "" {
		return "dev"
	}
	return version
}

// restore 把备份解包到 root。
//
// 它和运行服务时拿的是同一把锁。运行中的服务在内存里持有会话，
// 还会随时把状态写回磁盘；这时从底下改写 config.json，两边对这个
// 目录属于哪个账号的认识就会对不上。
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
		return errors.New("服务正在 " + root + " 上运行，先停掉它（systemctl stop mibot-lite）")
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	names, err := backup.Restore(archive, root, overwrite)
	if errors.Is(err, backup.ErrExists) {
		return errors.New(root + " 里已经有账号了；要用备份替换它，加上 --force")
	}
	if err != nil {
		return err
	}
	fmt.Printf("已把 %d 个文件恢复到 %s：\n", len(names), root)
	for _, name := range names {
		fmt.Println("  " + name)
	}
	return nil
}
