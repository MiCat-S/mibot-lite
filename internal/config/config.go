// Package config 读取部署目录：config.json（和 MiBox 写的是同一个文件，
// 已有的账号不用改就能直接用），以及可选的 .env，里面是几项 MIBOT_* 设置。
package config

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MaxConfigBytes 是能接受的 config.json 的最大字节数。
const MaxConfigBytes = 1 << 20

// DefaultDeviceModel 在 config.json 没有写 app_name 时使用。
const DefaultDeviceModel = "MiBot Lite"

// Proxy 是连接时可选的 SOCKS5 代理。
type Proxy struct {
	IP       string
	Port     int
	Username string
	Password string
}

// Config 是账号的启动配置。
type Config struct {
	APIID       int
	APIHash     string
	Session     string
	DeviceModel string
	Proxy       *Proxy
}

type rawConfig struct {
	APIID   json.Number `json:"api_id"`
	APIHash string      `json:"api_hash"`
	Session string      `json:"session"`
	AppName string      `json:"app_name"`
	Proxy   *struct {
		SocksType json.Number `json:"socksType"`
		IP        string      `json:"ip"`
		Port      json.Number `json:"port"`
		Username  *string     `json:"username"`
		Password  *string     `json:"password"`
	} `json:"proxy"`
}

// Parse 校验读进来的 config.json。
func Parse(raw []byte) (*Config, error) {
	var parsed rawConfig
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("config.json has trailing content")
	}
	apiID, err := parsed.APIID.Int64()
	if err != nil || apiID <= 0 {
		return nil, errors.New("config.json: api_id must be a positive integer")
	}
	if strings.TrimSpace(parsed.APIHash) == "" {
		return nil, errors.New("config.json: api_hash is required")
	}
	if strings.TrimSpace(parsed.Session) == "" {
		return nil, errors.New("config.json: session is required (run --login first)")
	}
	config := &Config{APIID: int(apiID), APIHash: parsed.APIHash, Session: parsed.Session, DeviceModel: DefaultDeviceModel}
	if strings.TrimSpace(parsed.AppName) != "" {
		config.DeviceModel = parsed.AppName
	}
	if parsed.Proxy != nil {
		socksType, err := parsed.Proxy.SocksType.Int64()
		if err != nil || socksType != 5 {
			return nil, errors.New("config.json: only proxy socksType 5 is supported")
		}
		port, err := parsed.Proxy.Port.Int64()
		if err != nil || port < 1 || port > 65535 {
			return nil, errors.New("config.json: proxy port must be between 1 and 65535")
		}
		if parsed.Proxy.IP == "" {
			return nil, errors.New("config.json: proxy ip is required")
		}
		proxy := &Proxy{IP: parsed.Proxy.IP, Port: int(port)}
		if parsed.Proxy.Username != nil {
			proxy.Username = *parsed.Proxy.Username
		}
		if parsed.Proxy.Password != nil {
			proxy.Password = *parsed.Proxy.Password
		}
		config.Proxy = proxy
	}
	return config, nil
}

// Read 从部署根目录加载 config.json。
func Read(root string) (*Config, error) {
	file, err := os.Open(filepath.Join(root, "config.json"))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxConfigBytes {
		return nil, errors.New("config.json must be a regular file within the size limit")
	}
	raw, err := io.ReadAll(io.LimitReader(file, MaxConfigBytes+1))
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Env 保存从 <root>/.env 和进程环境变量读到的可选设置。
// 两边都有时以进程环境变量为准。
type Env map[string]string

// ReadEnv 读取 <root>/.env 里的 KEY=value 行（文件不存在也没关系），
// 再用进程环境变量里的所有 MIBOT_* 变量覆盖上去。
func ReadEnv(root string, environ []string) Env {
	env := Env{}
	if file, err := os.Open(filepath.Join(root, ".env")); err == nil {
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
			env[key] = value
		}
		file.Close()
	}
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(key, "MIBOT_") {
			env[key] = value
		}
	}
	return env
}

// SetEnv 把 <root>/.env 里的 key 改成 value：已有这一行就原地替换（出现多次的都换掉），
// 没有就追加到末尾。其他行，包括注释和空行，原样保留；文件不存在就新建。
// 写入是原子的，权限 0600——.env 里可能有别的密钥。
//
// value 里不能有换行，也不能首尾是一对引号：ReadEnv 读的时候会把那对引号剥掉，
// 写进去的就不是读出来的了。调用方负责先检查。
func SetEnv(root, key, value string) error {
	path := filepath.Join(root, ".env")
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(raw) == 0 {
		lines = nil
	}
	replaced := false
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if name, _, ok := strings.Cut(trimmed, "="); ok && strings.TrimSpace(name) == key {
			lines[index] = key + "=" + value
			replaced = true
		}
	}
	if !replaced {
		lines = append(lines, key+"="+value)
	}
	temporary, err := os.CreateTemp(root, ".env-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

// Prefixes 返回命令前缀：优先用 MIBOT_PREFIX（以空格分隔），
// 没有设置就用 MiBox 的默认值。
func (e Env) Prefixes() []string {
	if configured := strings.Fields(e["MIBOT_PREFIX"]); len(configured) > 0 {
		return configured
	}
	return []string{".", "。", "$"}
}

// Get 返回某项设置，没有设置就返回默认值。
func (e Env) Get(key, fallback string) string {
	if value := strings.TrimSpace(e[key]); value != "" {
		return value
	}
	return fallback
}
