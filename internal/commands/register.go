// Package commands 把各命令接入应用。每个子目录是一个命令，或共用一套数据的一组命令
// （如 aban、ids、sudo）；kit 是它们共用的小工具。
package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	"github.com/MiCat-S/mibot-lite/internal/commands/privacy"
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
	privacy.Register(a)
	prefix.Register(a)
	alias.Register(a)
	sudo.Register(a)
}

// miboxImport 是一项迁移：MiBox 部署里的源文件、data/ 下的目标文件名，以及格式不同时的
// 转换函数（为 nil 表示格式相同，原样复制）。
type miboxImport struct {
	source, target string
	convert        func([]byte) ([]byte, error)
}

// miboxImports 按顺序处理；同一个目标有 v2 和 v1 两个来源时，v2 排在前面，
// 目标写过一次后面的就跳过——v2 的数据更新，而且 v2 启动时已经并入过 v1 的。
var miboxImports = []miboxImport{
	// ai：补上 MiBox v2 只在读取时才套用的默认值（openai 类型默认走 responses、搜索模型沿用对话模型）。
	{source: "assets/ai/config.json", target: "ai.json", convert: ai.ConvertMiBox},
	{source: "assets/sum/database.json", target: "sum.json"},
	{source: "assets/da/database.json", target: "da.json"},
	{source: "assets/dme/config.json", target: "dme.json"},
	// whois 只带得过来历史和 24 小时缓存；v2 的实际数据在 records.sqlite，不迁。
	{source: "assets/whois/whois_data.json", target: "whois.json"},
	{source: "assets/whois/data.json", target: "whois.json"},
	{source: "assets/aban/aban_cache.json", target: "aban.json"},
	{source: "assets/autochangename/autochangename.json", target: "acn.json"},
	{source: "assets/yvlu/config.json", target: "yvlu.json"},
	{source: "assets/t/tts_data.json", target: "t.json"},
	{source: "assets/sticker/config.json", target: "sticker.json"},
	{source: "assets/privacy/ip.json", target: "privacy.json"},
	// save：v2 存在自己的 config.json，v1 存在 prometheus 目录下，格式都要转换。
	{source: "assets/save/config.json", target: "save.json", convert: save.ConvertMiBox},
	{source: "assets/prometheus/config.json", target: "save.json", convert: save.ConvertMiBox},
	{source: "assets/speedtest/v2-config.json", target: "speedtest.json", convert: speedtest.ConvertMiBox},
	{source: "assets/speedtest/speedtest.json", target: "speedtest.json", convert: speedtest.ConvertMiBox},
	// 别名在 MiBox 里存在 SQLite 表 aliases(original, final)。
	{source: "assets/alias/alias.db", target: "alias.json", convert: func(raw []byte) ([]byte, error) {
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
	imported := 0
	written := map[string]bool{}
	for _, item := range miboxImports {
		if written[item.target] {
			continue
		}
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
		written[item.target] = true
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
