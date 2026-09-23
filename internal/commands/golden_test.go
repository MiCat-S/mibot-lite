package commands

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update 用来重写快照：确认行为是有意改变的之后，跑 go test ./internal/commands -update。
var update = flag.Bool("update", false, "用当前输出重写 testdata 里的快照")

// golden 把 got 和 testdata/<name>.golden 比较。
//
// 快照里存的是假 Telegram 收到的全部请求和最后存下来的配置。它们是拆分长函数时
// 用新旧两版逐条比对确认过的；之后任何改动只要改变了发出的请求或者存下的配置，
// 这里就会报出来，要么是 bug，要么是有意的改动——后者用 -update 重写快照。
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("缺少快照 %s，先用 -update 生成", path)
	}
	if string(want) == got {
		return
	}
	wantLines, gotLines := strings.Split(string(want), "\n"), strings.Split(got, "\n")
	for index := 0; index < len(wantLines) || index < len(gotLines); index++ {
		var expected, actual string
		if index < len(wantLines) {
			expected = wantLines[index]
		}
		if index < len(gotLines) {
			actual = gotLines[index]
		}
		if expected != actual {
			t.Fatalf("%s 和快照不一致，第 %d 行：\n快照：%s\n现在：%s\n（有意的改动请用 -update 重写快照）", name, index+1, expected, actual)
		}
	}
}

// section 把一个场景的记录排成快照里的一段。
func section(title string, calls []string, stored string) string {
	var out strings.Builder
	out.WriteString("## " + title + "\n")
	for _, call := range calls {
		out.WriteString(call + "\n")
	}
	if stored != "" {
		out.WriteString("-- 存下的配置 --\n" + stored + "\n")
	}
	return out.String() + "\n"
}
