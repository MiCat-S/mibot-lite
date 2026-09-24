package mibox

import (
	"os"
	"strings"
	"testing"
)

// testdata/alias.db 是用 Python 的 sqlite3 生成的真实数据库：403 行，
// 多页（有内部页），其中一行的值长到要用溢出页。
func TestAliases(t *testing.T) {
	data, err := os.ReadFile("testdata/alias.db")
	if err != nil {
		t.Fatal(err)
	}
	aliases, err := Aliases(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 403 {
		t.Fatalf("读出 %d 条，应为 403", len(aliases))
	}
	if aliases["测速"] != "speedtest 12345" || aliases["p"] != "ping" {
		t.Errorf("中文或短值读错：%q %q", aliases["测速"], aliases["p"])
	}
	if long := aliases["长"]; long != "sum run "+strings.Repeat("x", 5000) {
		t.Errorf("溢出页上的长值读错，长度 %d", len(long))
	}
	if aliases["a399"] != "ping "+strings.Repeat("y", 399%50) {
		t.Errorf("最后一行读错：%q", aliases["a399"])
	}
}

func TestRejectsNonSQLite(t *testing.T) {
	if _, err := Aliases([]byte("not a database")); err == nil {
		t.Error("不是 SQLite 文件应该报错")
	}
}

func TestVarint(t *testing.T) {
	for _, c := range []struct {
		bytes []byte
		value uint64
		used  int
	}{{[]byte{0x05}, 5, 1}, {[]byte{0x81, 0x00}, 128, 2}, {[]byte{0x82, 0x80, 0x01}, 0x8001, 3}} {
		if value, used := varint(c.bytes); value != c.value || used != c.used {
			t.Errorf("%x → %d %d，应为 %d %d", c.bytes, value, used, c.value, c.used)
		}
	}
}
