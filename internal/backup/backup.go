// Package backup 把一个部署的配置打成一个文件，再解到一个新目录里，
// 用于搬到新机器，或者在本机重装。
//
// 哪些算配置，只在这里决定，别处不再定义：
//
//   - config.json 和 gotd-session.json：账号。有了它们，恢复出来的部署
//     启动时不用重新登录。
//   - .env：命令前缀和其他 MIBOT_* 设置。
//   - data/ 下直接存放的每个 *.json：各个命令的设置，包括 API key。
//
// 其余的都是有意不带的。data/ 的子目录是缓存（eatgif 下载的帧、
// Speedtest CLI），第一次用到时会自己补回来。updates.json 是某一台机器
// 上连接的更新状态，带到另一台机器，只会让 Telegram 去重放一段根本
// 不存在的缺口。程序本身从发布版本获取，不从备份里来。
package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/config"
)

const (
	// Manifest 是每个归档的第一项。没有它的归档，Restore 一律拒绝：
	// 一个碰巧含有 config.json 的 tar 包，不能拿来覆盖一个账号。
	Manifest = "mibot-lite-backup.json"
	format   = 1

	// Restore 读取量的上限。真实的备份只有几十 KB；设这些上限，
	// 是为了不让恶意或传错的文件把磁盘写满。
	maxTotal   = 32 << 20
	maxEntries = 512
)

// 部署根目录下的账号和设置文件。
var rootFiles = []string{"config.json", "gotd-session.json", ".env"}

// stale 列出的文件，如果归档里没有新的来替换，Restore 就把它删掉。
//
// 最要紧的是 gotd-session.json。启动时，只有这个文件不存在，才会导入
// config.json 里的会话；如果留着一个旧的，就以旧的为准。把某人的
// config.json 恢复到一个还放着另一个账号 gotd-session.json 的目录里，
// 程序会悄无声息地继续以那个账号运行。
var stale = []string{"gotd-session.json", "updates.json"}

// ErrExists 表示目标目录里已经有账号，而调用方没有要求替换。
var ErrExists = errors.New("this directory already has an account; restoring would replace it")

type manifest struct {
	Format  int       `json:"format"`
	Version string    `json:"version"`
	Created time.Time `json:"created"`
	Files   []string  `json:"files"`
}

// Create 把 root 下的配置打包，返回 gzip 压缩的 tar 包，以及其中包含的
// 文件名（不算清单文件）。
func Create(root, version string, now time.Time) ([]byte, []string, error) {
	var names []string
	for _, name := range rootFiles {
		if regular(filepath.Join(root, name)) {
			names = append(names, name)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "data"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	for _, entry := range entries {
		name := "data/" + entry.Name()
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json") && allowed(name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, nil, errors.New("nothing to back up: no config.json, .env or data/*.json here")
	}
	sort.Strings(names)

	var archive bytes.Buffer
	zipped := gzip.NewWriter(&archive)
	packed := tar.NewWriter(zipped)
	head, err := json.MarshalIndent(manifest{Format: format, Version: version, Created: now.UTC(), Files: names}, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	if err := put(packed, Manifest, head, now); err != nil {
		return nil, nil, err
	}
	for _, name := range names {
		full := filepath.Join(root, filepath.FromSlash(name))
		body, err := os.ReadFile(full)
		if err != nil {
			return nil, nil, err
		}
		info, err := os.Stat(full)
		if err != nil {
			return nil, nil, err
		}
		if err := put(packed, name, body, info.ModTime()); err != nil {
			return nil, nil, err
		}
	}
	if err := packed.Close(); err != nil {
		return nil, nil, err
	}
	if err := zipped.Close(); err != nil {
		return nil, nil, err
	}
	return archive.Bytes(), names, nil
}

func put(packed *tar.Writer, name string, body []byte, stamp time.Time) error {
	header := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), ModTime: stamp, Typeflag: tar.TypeReg, Format: tar.FormatPAX}
	if err := packed.WriteHeader(header); err != nil {
		return err
	}
	_, err := packed.Write(body)
	return err
}

func regular(name string) bool {
	info, err := os.Lstat(name)
	return err == nil && info.Mode().IsRegular()
}

// allowed 判断归档里的某个路径是不是 Create 会写入的。Restore 每一项
// 都用它检查，所以归档永远没法把文件放到别处：既放不到程序旁边，
// 也跳不出部署目录。
func allowed(name string) bool {
	if name != path.Clean(name) || strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
		return false
	}
	for _, top := range rootFiles {
		if name == top {
			return true
		}
	}
	rest, ok := strings.CutPrefix(name, "data/")
	return ok && rest != "" && !strings.Contains(rest, "/") &&
		strings.HasSuffix(rest, ".json") && !strings.HasPrefix(rest, ".")
}

// Restore 把 Create 生成的归档解到 root 下，返回写入的文件名。
//
// 整个归档读完、检查完之前什么都不写，所以下载不完整，或者文件根本
// 不是本程序的备份时，目录会原样保留。
func Restore(archive io.Reader, root string, overwrite bool) ([]string, error) {
	zipped, err := gzip.NewReader(archive)
	if err != nil {
		return nil, errors.New("not a mibot-lite backup: the file is not gzip")
	}
	defer zipped.Close()
	limited := &io.LimitedReader{R: zipped, N: maxTotal + 1}
	packed := tar.NewReader(limited)

	files := map[string][]byte{}
	var head *manifest
	for count := 0; ; count++ {
		header, err := packed.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the backup is damaged: %w", err)
		}
		if count >= maxEntries {
			return nil, errors.New("the backup has too many entries to be one of ours")
		}
		if header.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("the backup contains %q, which is not a plain file", header.Name)
		}
		body, err := io.ReadAll(packed)
		if err != nil {
			return nil, fmt.Errorf("the backup is damaged: %w", err)
		}
		if limited.N <= 0 {
			return nil, errors.New("the backup is larger than any configuration could be")
		}
		if header.Name == Manifest {
			head = new(manifest)
			if err := json.Unmarshal(body, head); err != nil {
				return nil, errors.New("the backup's manifest is unreadable")
			}
			continue
		}
		if !allowed(header.Name) {
			return nil, fmt.Errorf("the backup contains %q, which a mibot-lite backup never holds", header.Name)
		}
		if _, seen := files[header.Name]; seen {
			return nil, fmt.Errorf("the backup contains %q twice", header.Name)
		}
		files[header.Name] = body
	}
	if head == nil {
		return nil, errors.New("not a mibot-lite backup: it has no " + Manifest)
	}
	if head.Format > format {
		return nil, fmt.Errorf("this backup was made by a newer mibot-lite (format %d); upgrade first", head.Format)
	}
	if len(files) == 0 {
		return nil, errors.New("the backup is empty")
	}
	if body, ok := files["config.json"]; ok {
		if _, err := config.Parse(body); err != nil {
			return nil, fmt.Errorf("the backup's config.json is not usable: %w", err)
		}
	}
	if !overwrite && regular(filepath.Join(root, "config.json")) {
		return nil, ErrExists
	}

	if err := os.MkdirAll(filepath.Join(root, "data"), 0o700); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := writeAtomic(filepath.Join(root, filepath.FromSlash(name)), files[name]); err != nil {
			return nil, err
		}
	}
	for _, name := range stale {
		if _, replaced := files[name]; !replaced {
			if err := os.Remove(filepath.Join(root, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
	}
	return names, nil
}

func writeAtomic(target string, body []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(target), ".restore-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(body); err != nil {
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
	return os.Rename(temporary.Name(), target)
}
