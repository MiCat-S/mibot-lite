package update

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

func TestNewerVersion(t *testing.T) {
	if !newer("", "v1.0.0") || !newer("dev", "v1.0.0") || !newer("0.1.0", "v0.2.0") {
		t.Error("a new release should be detected")
	}
	if newer("v1.2.3", "1.2.3") {
		t.Error("the same version with a different v prefix is not newer")
	}
}

// swapBinary 的每一步失败，都要让程序文件的位置上还有一个能跑的版本；实在恢复不了，
// 也要告诉人原来的版本在哪里。rename 换成在第 fail 次调用时失败的假版本。
func TestSwapBinary(t *testing.T) {
	setup := func(t *testing.T) (string, string, string) {
		dir := t.TempDir()
		binary, incoming, aside := filepath.Join(dir, "mibot-lite"), filepath.Join(dir, "mibot-lite.download"), filepath.Join(dir, "mibot-lite.previous")
		os.WriteFile(binary, []byte("old"), 0o755)
		os.WriteFile(incoming, []byte("new"), 0o755)
		return binary, incoming, aside
	}
	failing := func(fail ...int) func(from, to string) error {
		calls := 0
		return func(from, to string) error {
			calls++
			for _, n := range fail {
				if calls == n {
					return errors.New("rename failed")
				}
			}
			return os.Rename(from, to)
		}
	}
	read := func(path string) string {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "<missing>"
		}
		return string(raw)
	}

	binary, incoming, aside := setup(t)
	if err := swapBinary(os.Rename, binary, incoming, aside); err != nil || read(binary) != "new" || read(aside) != "old" {
		t.Fatalf("swap: err=%v binary=%s aside=%s", err, read(binary), read(aside))
	}

	// 挪不开当前的：什么都没动。
	binary, incoming, aside = setup(t)
	err := swapBinary(failing(1), binary, incoming, aside)
	if text, ok := kit.IsUserError(err); !ok || !strings.Contains(text, "什么都没改") || read(binary) != "old" {
		t.Errorf("first step: %v binary=%s", err, read(binary))
	}

	// 新的放不进去：换回原来的。
	binary, incoming, aside = setup(t)
	err = swapBinary(failing(2), binary, incoming, aside)
	if text, ok := kit.IsUserError(err); !ok || !strings.Contains(text, "已恢复") || read(binary) != "old" {
		t.Errorf("second step: %v binary=%s", err, read(binary))
	}

	// 放不进去、也换不回来：程序文件的位置是空的，提示里要写明原来的版本在哪、该改成什么名字。
	binary, incoming, aside = setup(t)
	err = swapBinary(failing(2, 3), binary, incoming, aside)
	text, ok := kit.IsUserError(err)
	if !ok || !strings.Contains(text, aside) || !strings.Contains(text, binary) || read(aside) != "old" {
		t.Errorf("restore failed: %q (aside=%s)", text, read(aside))
	}
	if !strings.Contains(err.Error(), "rename failed") {
		t.Errorf("the log line lost the cause: %v", err)
	}
}
