package commands

import (
	"io"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/sudo"
)

// readmeRow 是 README 命令表里的一行：第一格里出现的命令名，和最后一格「可借」。
type readmeRow struct {
	group int
	names []string
	lend  string
	line  string
}

// readmeLanguages 是四份 README，和各自「命令」一节的标题、「可借」一栏的写法。
var readmeLanguages = []struct {
	file    string
	heading string
	partly  string
}{
	{"README.md", "## 命令", "部分"},
	{"README.en.md", "## Commands", "partly"},
	{"README.zh-TW.md", "## 指令", "部分"},
	{"README.ja.md", "## コマンド", "一部"},
}

var readmeCommand = regexp.MustCompile("`\\.([a-z0-9]+)")

// readmeTables 读出「命令」一节里各分组的表格；分组按出现顺序编号，对应 command.Groups。
func readmeTables(t *testing.T, file, heading, partly string) []readmeRow {
	t.Helper()
	raw, err := os.ReadFile("../../" + file)
	if err != nil {
		t.Fatal(err)
	}
	var rows []readmeRow
	inSection, group, inTable := false, -1, false
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "## "):
			inSection = line == heading
			continue
		case !inSection:
			continue
		case strings.HasPrefix(line, "**") && strings.HasSuffix(line, "**"):
			group++
			inTable = false
			continue
		case !strings.HasPrefix(line, "|"):
			continue
		}
		if !inTable {
			inTable = true // 表头
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), " | ")
		if strings.HasPrefix(cells[0], "---") {
			continue
		}
		var names []string
		for _, match := range readmeCommand.FindAllStringSubmatch(cells[0], -1) {
			names = append(names, match[1])
		}
		lend := "none"
		switch strings.TrimSpace(cells[len(cells)-1]) {
		case "✓":
			lend = "all"
		case partly:
			lend = "partly"
		}
		rows = append(rows, readmeRow{group: group, names: names, lend: lend, line: line})
	}
	return rows
}

// README 的命令表要和代码对得上：每条命令（连同简写）都在它所属分组的表里出现一次，
// 「可借」一栏和借用规则一致。四种语言各查一遍。以前 README 和 .help 各写各的，
// .tr 能不能借、简写有哪些都对不上过。
func TestREADMEMatchesRegistry(t *testing.T) {
	a := &app.App{Root: t.TempDir(), Registry: command.New([]string{"."}, slog.New(slog.NewTextHandler(io.Discard, nil)))}
	RegisterAll(a)
	for _, language := range readmeLanguages {
		rows := readmeTables(t, language.file, language.heading, language.partly)
		if len(rows) == 0 {
			t.Fatalf("%s：没找到「%s」一节的表格", language.file, language.heading)
		}
		seen := map[string]int{}
		for _, row := range rows {
			if row.group < 0 || row.group >= len(command.Groups) {
				t.Errorf("%s：表格多于 %d 组：%s", language.file, len(command.Groups), row.line)
				continue
			}
			for _, name := range row.names {
				cmd, ok := a.Registry.Lookup(name)
				if !ok {
					t.Errorf("%s：没有 .%s 这个命令：%s", language.file, name, row.line)
					continue
				}
				seen[name]++
				if cmd.Group != command.Groups[row.group] {
					t.Errorf("%s：.%s 属于「%s」，却写在第 %d 组的表里", language.file, name, cmd.Group, row.group+1)
				}
				if cmd.Name == name {
					if got := sudo.Lending(name); got != row.lend {
						t.Errorf("%s：.%s 的「可借」写成 %s，实际是 %s", language.file, name, row.lend, got)
					}
					for _, alias := range cmd.Aliases {
						if !slices.Contains(row.names, alias) {
							t.Errorf("%s：.%s 那一行没写简写 .%s", language.file, name, alias)
						}
					}
				}
			}
		}
		for _, cmd := range a.Registry.Commands() {
			if seen[cmd.Name] != 1 {
				t.Errorf("%s：.%s 在命令表里出现了 %d 次，应为 1 次", language.file, cmd.Name, seen[cmd.Name])
			}
		}
	}
}

// 每条命令都要归到一个分组，说明写成列表里的一句话。
func TestCommandsHaveGroupAndDescription(t *testing.T) {
	a := &app.App{Root: t.TempDir(), Registry: command.New([]string{"."}, slog.New(slog.NewTextHandler(io.Discard, nil)))}
	RegisterAll(a)
	for _, cmd := range a.Registry.Commands() {
		if !slices.Contains(command.Groups, cmd.Group) {
			t.Errorf(".%s 没有分组（%q）", cmd.Name, cmd.Group)
		}
	}
}
