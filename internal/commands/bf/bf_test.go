package bf

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// 说明文字跟着备份文件本身一起发，不管有多少个命令配置文件，都得控制在
// Telegram 的 1024 字符以内。
func TestBackupCaptionFits(t *testing.T) {
	names := []string{"config.json", "gotd-session.json", ".env"}
	for i := 0; i < 40; i++ {
		names = append(names, fmt.Sprintf("data/command%02d.json", i))
	}
	caption := backupCaption("0.1.12", names, 48000)
	plain := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(caption, "")
	if n := len([]rune(plain)); n > 1024 {
		t.Errorf("caption is %d characters, over Telegram's 1024", n)
	}
	for _, wanted := range []string{"--restore", "不要转发", "43 个文件"} {
		if !strings.Contains(caption, wanted) {
			t.Errorf("caption is missing %q:\n%s", wanted, plain)
		}
	}
}

func TestBackupHelpSaysWhereItGoesAndHowToRestore(t *testing.T) {
	text := backupHelp(".")
	for _, wanted := range []string{"收藏夹", "--restore", "--force", ".log"} {
		if !strings.Contains(text, wanted) {
			t.Errorf("the help never mentions %q", wanted)
		}
	}
}
