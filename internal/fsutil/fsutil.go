// Package fsutil 是和文件系统打交道的小工具。
package fsutil

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic 把 data 写进 path：先写到同目录下一个随机命名的临时文件，落盘（fsync）后
// 再改名过去。读的人要么看到旧文件，要么看到完整的新文件，不会读到写了一半的；两个进程
// 或协程同时写同一个文件，也不会像固定的 .tmp 文件名那样互相截断。目录不存在就建。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	fail := func(err error) error {
		temporary.Close()
		os.Remove(name)
		return err
	}
	if err := temporary.Chmod(perm); err != nil {
		return fail(err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fail(err)
	}
	if err := temporary.Sync(); err != nil {
		return fail(err)
	}
	if err := temporary.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
