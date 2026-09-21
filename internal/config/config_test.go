package config

import (
	"os"
	"path/filepath"
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
