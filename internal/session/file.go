package session

import (
	"context"
	"errors"
	"os"

	gotdsession "github.com/gotd/td/session"

	"github.com/MiCat-S/mibot-lite/internal/fsutil"
)

// FileStorage 保存 gotd 格式的会话。服务重连更新文件时，.bf 和 --backup 仍会读到完整会话。
type FileStorage struct{ Path string }

func (f FileStorage) LoadSession(_ context.Context) ([]byte, error) {
	data, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, gotdsession.ErrNotFound
	}
	return data, err
}

func (f FileStorage) StoreSession(_ context.Context, data []byte) error {
	return fsutil.WriteFileAtomic(f.Path, data, 0o600)
}
