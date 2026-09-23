package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAcceptsMiBoxConfig(t *testing.T) {
	config, err := Parse([]byte(`{"api_id": 1234567, "api_hash": "abc", "session": "1xyz"}`))
	if err != nil {
		t.Fatal(err)
	}
	if config.APIID != 1234567 || config.APIHash != "abc" || config.Session != "1xyz" {
		t.Fatalf("unexpected config: %+v", config)
	}
	if config.DeviceModel != DefaultDeviceModel {
		t.Fatalf("device model %q", config.DeviceModel)
	}
}

func TestParseRejectsIncomplete(t *testing.T) {
	for name, raw := range map[string]string{
		"no api_id":  `{"api_hash":"a","session":"1x"}`,
		"no hash":    `{"api_id":1,"session":"1x"}`,
		"no session": `{"api_id":1,"api_hash":"a"}`,
		"trailing":   `{"api_id":1,"api_hash":"a","session":"1x"} junk`,
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseProxy(t *testing.T) {
	config, err := Parse([]byte(`{"api_id":1,"api_hash":"a","session":"1x","proxy":{"socksType":5,"ip":"1.2.3.4","port":1080,"username":"u","password":"p"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if config.Proxy == nil || config.Proxy.IP != "1.2.3.4" || config.Proxy.Port != 1080 || config.Proxy.Username != "u" {
		t.Fatalf("unexpected proxy: %+v", config.Proxy)
	}
	if _, err := Parse([]byte(`{"api_id":1,"api_hash":"a","session":"1x","proxy":{"socksType":4,"ip":"1.2.3.4","port":1080}}`)); err == nil {
		t.Error("socks4 should be rejected")
	}
}

func TestEnvFileAndPrefixes(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("# comment\nMIBOT_PREFIX=! ！\nMIBOT_SERVICE=\"custom.service\"\n\nBROKEN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := ReadEnv(root, []string{"MIBOT_UPDATE_REPO=owner/repo", "PATH=/bin"})
	if got := env.Prefixes(); len(got) != 2 || got[0] != "!" || got[1] != "！" {
		t.Fatalf("prefixes %v", got)
	}
	if env.Get("MIBOT_SERVICE", "") != "custom.service" {
		t.Fatalf("quoted value not unwrapped: %q", env["MIBOT_SERVICE"])
	}
	if env.Get("MIBOT_UPDATE_REPO", "") != "owner/repo" {
		t.Error("process environment should be visible")
	}
	if env.Get("PATH", "unset") != "unset" {
		t.Error("only MIBOT_* variables come from the environment")
	}
	if got := ReadEnv(t.TempDir(), nil).Prefixes(); len(got) != 3 || got[0] != "." {
		t.Fatalf("default prefixes %v", got)
	}
}

// SetEnv 只动那一行：注释、空行、别的设置原样保留，改完 ReadEnv 读出来的就是写进去的。
func TestSetEnvChangesOnlyThatLine(t *testing.T) {
	root := t.TempDir()
	original := "# 部署设置\nMIBOT_SERVICE=\"custom.service\"\n\nMIBOT_PREFIX=. 。\nOTHER=1\n"
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetEnv(root, "MIBOT_PREFIX", "! ！ ~"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, ".env"))
	want := "# 部署设置\nMIBOT_SERVICE=\"custom.service\"\n\nMIBOT_PREFIX=! ！ ~\nOTHER=1\n"
	if string(raw) != want {
		t.Fatalf(".env 变成了：\n%s\n应为：\n%s", raw, want)
	}
	if got := strings.Join(ReadEnv(root, nil).Prefixes(), " "); got != "! ！ ~" {
		t.Errorf("读回来的前缀是 %q", got)
	}
	info, _ := os.Stat(filepath.Join(root, ".env"))
	if info.Mode().Perm() != 0o600 {
		t.Errorf(".env 权限是 %v，应为 0600", info.Mode().Perm())
	}
}

func TestSetEnvAppendsOrCreates(t *testing.T) {
	root := t.TempDir()
	if err := SetEnv(root, "MIBOT_PREFIX", "!"); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(root, ".env")); string(raw) != "MIBOT_PREFIX=!\n" {
		t.Errorf("新建的 .env 是 %q", raw)
	}
	os.WriteFile(filepath.Join(root, ".env"), []byte("OTHER=1"), 0o600)
	if err := SetEnv(root, "MIBOT_PREFIX", "$"); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(root, ".env")); string(raw) != "OTHER=1\nMIBOT_PREFIX=$\n" {
		t.Errorf("追加后的 .env 是 %q", raw)
	}
}
