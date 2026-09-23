package commands

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/MiCat-S/mibot-lite/internal/app"
)

// RegisterAll 把所有命令组接入应用。
func RegisterAll(a *app.App) {
	Core(a)
	Restart(a)
	Update(a)
	Calc(a)
	Rate(a)
	Whois(a)
	AI(a)
	Gt(a)
	Translate(a)
	Speedtest(a)
	Log(a)
	Backup(a)
	Save(a)
	Lookup(a)
	Entity(a)
	Sum(a)
	Re(a)
	Dme(a)
	Da(a)
	Aban(a)
	Acn(a)
	Yvlu(a)
	Eatgif(a)
	Prefix(a)
	Alias(a)
	Delegate(a)
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
