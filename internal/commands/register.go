// Package commands 把各命令接入应用。每个子目录是一个命令，或共用一套数据的一组命令
// （如 aban、ids、sudo）；kit 是它们共用的小工具。
package commands

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/commands/aban"
	"github.com/MiCat-S/mibot-lite/internal/commands/acn"
	"github.com/MiCat-S/mibot-lite/internal/commands/ai"
	"github.com/MiCat-S/mibot-lite/internal/commands/alias"
	"github.com/MiCat-S/mibot-lite/internal/commands/bf"
	"github.com/MiCat-S/mibot-lite/internal/commands/bin"
	"github.com/MiCat-S/mibot-lite/internal/commands/calc"
	"github.com/MiCat-S/mibot-lite/internal/commands/core"
	"github.com/MiCat-S/mibot-lite/internal/commands/da"
	"github.com/MiCat-S/mibot-lite/internal/commands/dme"
	"github.com/MiCat-S/mibot-lite/internal/commands/eatgif"
	"github.com/MiCat-S/mibot-lite/internal/commands/gt"
	"github.com/MiCat-S/mibot-lite/internal/commands/ids"
	"github.com/MiCat-S/mibot-lite/internal/commands/ip"
	"github.com/MiCat-S/mibot-lite/internal/commands/log"
	"github.com/MiCat-S/mibot-lite/internal/commands/prefix"
	"github.com/MiCat-S/mibot-lite/internal/commands/rate"
	"github.com/MiCat-S/mibot-lite/internal/commands/re"
	"github.com/MiCat-S/mibot-lite/internal/commands/restart"
	"github.com/MiCat-S/mibot-lite/internal/commands/save"
	"github.com/MiCat-S/mibot-lite/internal/commands/speedtest"
	"github.com/MiCat-S/mibot-lite/internal/commands/sticker"
	"github.com/MiCat-S/mibot-lite/internal/commands/sudo"
	"github.com/MiCat-S/mibot-lite/internal/commands/sum"
	"github.com/MiCat-S/mibot-lite/internal/commands/tr"
	"github.com/MiCat-S/mibot-lite/internal/commands/tts"
	"github.com/MiCat-S/mibot-lite/internal/commands/update"
	"github.com/MiCat-S/mibot-lite/internal/commands/whois"
	"github.com/MiCat-S/mibot-lite/internal/commands/yvlu"
)

// RegisterAll 把所有命令组接入应用。
func RegisterAll(a *app.App) {
	core.Register(a)
	restart.Register(a)
	update.Register(a)
	calc.Register(a)
	rate.Register(a)
	whois.Register(a)
	ai.Register(a)
	gt.Register(a)
	tr.Register(a)
	speedtest.Register(a)
	log.Register(a)
	bf.Register(a)
	save.Register(a)
	ip.Register(a)
	bin.Register(a)
	ids.Register(a)
	sum.Register(a)
	re.Register(a)
	dme.Register(a)
	da.Register(a)
	aban.Register(a)
	acn.Register(a)
	yvlu.Register(a)
	eatgif.Register(a)
	sticker.Register(a)
	tts.Register(a)
	prefix.Register(a)
	alias.Register(a)
	sudo.Register(a)
}

// miboxFiles 把 MiBox 插件的数据文件映射到 data/ 下的文件名。JSON 结构
// 和 MiBox 插件写的一样，所以原样复制即可。
var miboxFiles = map[string]string{
	"assets/ai/config.json":                     "ai.json",
	"assets/sum/database.json":                  "sum.json",
	"assets/da/database.json":                   "da.json",
	"assets/dme/config.json":                    "dme.json",
	"assets/whois/data.json":                    "whois.json",
	"assets/aban/aban_cache.json":               "aban.json",
	"assets/autochangename/autochangename.json": "acn.json",
	"assets/yvlu/config.json":                   "yvlu.json",
	"assets/t/tts_data.json":                    "t.json",
	"assets/sticker/config.json":                "sticker.json",
}

// ImportMiBox 从 MiBox 部署中复制插件数据文件。
func ImportMiBox(miboxRoot, dataDir string, out io.Writer) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	copied := 0
	for source, target := range miboxFiles {
		raw, err := os.ReadFile(filepath.Join(miboxRoot, source))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		destination := filepath.Join(dataDir, target)
		if _, err := os.Stat(destination); err == nil {
			fmt.Fprintf(out, "skip %s (already exists)\n", destination)
			continue
		}
		if err := os.WriteFile(destination, raw, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(out, "copied %s -> %s\n", source, destination)
		copied++
	}
	fmt.Fprintf(out, "%d file(s) imported\n", copied)
	return nil
}
