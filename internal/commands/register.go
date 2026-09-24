// Package commands 把各命令接入应用。每个子目录是一个命令，或共用一套数据的一组命令
// （如 aban、ids、sudo）；kit 是它们共用的小工具。
package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
	"github.com/MiCat-S/mibot-lite/internal/config"
	"github.com/MiCat-S/mibot-lite/internal/mibox"
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

// converters 是要先转换格式再写入的 MiBox 数据：源文件 → (data/ 下的文件名, 转换函数)。
var converters = map[string]struct {
	target  string
	convert func([]byte) ([]byte, error)
}{
	// 别名在 MiBox 里存在 SQLite 表 aliases(original, final)。
	"assets/alias/alias.db": {"alias.json", func(raw []byte) ([]byte, error) {
		aliases, err := mibox.Aliases(raw)
		if err != nil {
			return nil, err
		}
		return json.MarshalIndent(map[string]any{"aliases": aliases}, "", "  ")
	}},
}

// ImportMiBox 把 MiBox 部署里的插件数据搬到 dataDir：格式相同的原样复制，
// 不同的先转换；再把 MiBox .env 里的 TB_PREFIX 写成这边的 MIBOT_PREFIX。
// 目标已存在的文件、已设置的前缀都不覆盖。
func ImportMiBox(miboxRoot, dataDir string, out io.Writer) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	type job struct {
		source, target string
		convert        func([]byte) ([]byte, error)
	}
	var jobs []job
	for source, target := range miboxFiles {
		jobs = append(jobs, job{source: source, target: target})
	}
	for source, converter := range converters {
		jobs = append(jobs, job{source: source, target: converter.target, convert: converter.convert})
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].source < jobs[j].source })
	imported := 0
	for _, item := range jobs {
		raw, err := os.ReadFile(filepath.Join(miboxRoot, item.source))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		destination := filepath.Join(dataDir, item.target)
		if _, err := os.Stat(destination); err == nil {
			fmt.Fprintf(out, "skip %s (already exists)\n", destination)
			continue
		}
		if item.convert != nil {
			// SQLite 开着 WAL 时，最近的写入可能还在 -wal 文件里没合并，只读主文件会漏掉。
			if info, err := os.Stat(filepath.Join(miboxRoot, item.source+"-wal")); err == nil && info.Size() > 0 {
				fmt.Fprintf(out, "warning: %s-wal exists; stop MiBox first so recent changes are merged into %s\n", item.source, item.source)
			}
			converted, err := item.convert(raw)
			if err != nil {
				fmt.Fprintf(out, "skip %s: %v\n", item.source, err)
				continue
			}
			raw = converted
		}
		if err := os.WriteFile(destination, raw, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(out, "imported %s -> %s\n", item.source, destination)
		imported++
	}
	fmt.Fprintf(out, "%d file(s) imported\n", imported)
	return importPrefix(miboxRoot, filepath.Dir(dataDir), out)
}

// importPrefix 把 MiBox 的 TB_PREFIX（空格分隔的前缀）写成 MIBOT_PREFIX，
// 这边已经设过前缀就不动。没有这一步，自定义过前缀的人换过来后命令全都没反应。
func importPrefix(miboxRoot, root string, out io.Writer) error {
	prefixes := config.ReadEnv(miboxRoot, nil)["TB_PREFIX"]
	if strings.TrimSpace(prefixes) == "" {
		return nil
	}
	if _, set := config.ReadEnv(root, nil)["MIBOT_PREFIX"]; set {
		fmt.Fprintf(out, "skip TB_PREFIX (MIBOT_PREFIX already set)\n")
		return nil
	}
	value := strings.Join(strings.Fields(prefixes), " ")
	if err := config.SetEnv(root, "MIBOT_PREFIX", value); err != nil {
		return err
	}
	fmt.Fprintf(out, "imported TB_PREFIX -> MIBOT_PREFIX=%s\n", value)
	return nil
}
