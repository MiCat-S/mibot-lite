// Package store 是持久化层：每类命令一个 JSON 文件，原子写入，
// 用互斥锁保护。不用数据库。
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/MiCat-S/mibot-lite/internal/fsutil"
)

// Store 在磁盘上保存一个类型为 T 的 JSON 文档。
type Store[T any] struct {
	path     string
	defaults func() T
	mu       sync.Mutex
	cached   atomic.Pointer[T]
}

// New 创建一个 store。defaults 生成文件不存在或为空时所代表的文档；
// 每次调用都必须返回一个新值。
func New[T any](path string, defaults func() T) *Store[T] {
	return &Store[T]{path: path, defaults: defaults}
}

// Path 是文档所在的路径。
func (s *Store[T]) Path() string { return s.path }

func (s *Store[T]) load() (*T, error) {
	if cached := s.cached.Load(); cached != nil {
		return cached, nil
	}
	value := s.defaults()
	raw, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	case len(raw) > 0:
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("%s: %w", s.path, err)
		}
	}
	s.cached.Store(&value)
	return &value, nil
}

// Snapshot 返回最近一次成功写入的只读快照。Update 修改独立副本，已有快照始终有效。
// 需要修改结果的调用方使用 Read。
func (s *Store[T]) Snapshot() (*T, error) {
	// 监听器每条消息都读规则，已有快照时无需等待正在进行的文件写入。
	if cached := s.cached.Load(); cached != nil {
		return cached, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// Read 返回文档的一份副本。
func (s *Store[T]) Read() (T, error) {
	value, err := s.Snapshot()
	if err != nil {
		var zero T
		return zero, err
	}
	return clone(*value)
}

// Update 用 mutate 修改文档再写回。mutate 返回错误时，文件保持不动。
func (s *Store[T]) Update(mutate func(value *T) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.load()
	if err != nil {
		return err
	}
	working, err := clone(*current)
	if err != nil {
		return err
	}
	if err := mutate(&working); err != nil {
		return err
	}
	if err := writeAtomic(s.path, working); err != nil {
		return err
	}
	s.cached.Store(&working)
	return nil
}

func clone[T any](value T) (T, error) {
	var out T
	raw, err := json.Marshal(value)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	return out, nil
}

func writeAtomic(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", " ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(encoded, '\n'), 0o600)
}
