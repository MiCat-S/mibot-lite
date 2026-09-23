// Package backup packs a deployment's configuration into one file and
// unpacks it into a fresh directory — for moving to a new machine, or
// reinstalling this one.
//
// What counts as configuration is decided here and nowhere else:
//
//   - config.json and gotd-session.json: the account. With them a restored
//     deployment starts without signing in again.
//   - .env: the prefix and the other MIBOT_* settings.
//   - every *.json directly in data/: each command's settings, API keys
//     included.
//
// Everything else is left out on purpose. The subdirectories of data/ are
// caches — eatgif's downloaded frames, the Speedtest CLI — and come back
// by themselves the first time they are needed. updates.json is the update
// state of one machine's connection; carried to another it would only ask
// Telegram to replay a gap that is not there. The binary comes from the
// release, not the backup.
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
	// Manifest is the first entry of every archive. Restore refuses an
	// archive without it: a tarball that merely happens to contain a
	// config.json is not something to unpack over an account.
	Manifest = "mibot-lite-backup.json"
	format   = 1

	// Bounds on what Restore reads. A real backup is tens of kilobytes;
	// these exist so a hostile or mistaken file cannot fill the disk.
	maxTotal   = 32 << 20
	maxEntries = 512
)

// The account and the settings, at the top of the deployment.
var rootFiles = []string{"config.json", "gotd-session.json", ".env"}

// Stale is what Restore deletes when the archive does not replace it.
//
// gotd-session.json matters most. On start the session in config.json is
// only imported when that file is missing; if an older one is lying
// around, it wins. Restoring someone's config.json over a directory that
// still holds another account's gotd-session.json would quietly go on
// running as the other account.
var stale = []string{"gotd-session.json", "updates.json"}

// ErrExists is returned when the target already holds an account and the
// caller did not ask to replace it.
var ErrExists = errors.New("this directory already has an account; restoring would replace it")

type manifest struct {
	Format  int       `json:"format"`
	Version string    `json:"version"`
	Created time.Time `json:"created"`
	Files   []string  `json:"files"`
}

// Create packs the configuration under root. It returns the gzipped tar
// and the names it holds, not counting the manifest.
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

// allowed reports whether a path inside an archive is one Create writes.
// Restore holds every entry to it, so an archive can never place a file
// anywhere else — not beside the binary, not above the directory.
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

// Restore unpacks an archive made by Create into root and returns the
// names it wrote.
//
// Nothing is written until the whole archive has been read and checked,
// so a truncated download or a foreign file leaves the directory exactly
// as it was.
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
