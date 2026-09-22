package commands

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/MiCat-S/mibot-lite/internal/app"
)

// RegisterAll wires every command family into the app.
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
	Sum(a)
	Re(a)
	Dme(a)
	Da(a)
	Aban(a)
	Acn(a)
	Yvlu(a)
	Eatgif(a)
}

// miboxFiles maps MiBox plugin data files to their names under data/. The
// JSON shapes are the ones the MiBox plugins wrote, so the copy is verbatim.
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

// ImportMiBox copies the plugin data files from a MiBox deployment.
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
