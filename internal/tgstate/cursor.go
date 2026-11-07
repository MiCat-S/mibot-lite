package tgstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// cursorDocument 不含长期保存的 hash；每秒落盘的大小只随已同步频道数增长。
// Generation 对应完整快照；旧版重写或新版刷新快照后，不会误用之前留下的游标。
type cursorDocument struct {
	Generation uint64         `json:"generation"`
	UserID     int64          `json:"userId"`
	Pts        int            `json:"pts"`
	Qts        int            `json:"qts"`
	Date       int            `json:"date"`
	Seq        int            `json:"seq"`
	HasState   bool           `json:"hasState"`
	Channels   map[string]int `json:"channels"`
}

func (s *State) cursor() cursorDocument {
	d := &s.doc
	return cursorDocument{Generation: d.Generation, UserID: d.UserID, Pts: d.Pts,
		Qts: d.Qts, Date: d.Date, Seq: d.Seq, HasState: d.HasState, Channels: d.Channels}
}

func (s *State) loadCursor() error {
	// 没有新版完整快照时，以旧版文件为准；也保证删除状态文件后不会复活旧游标。
	if s.doc.Generation == 0 {
		return nil
	}
	raw, err := os.ReadFile(s.path + ".cursor")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read update cursor: %w", err)
	}
	var c cursorDocument
	if err := json.Unmarshal(raw, &c); err != nil {
		return fmt.Errorf("decode update cursor: %w", err)
	}
	if c.Generation != s.doc.Generation {
		return nil
	}
	s.doc.UserID, s.doc.HasState = c.UserID, c.HasState
	s.doc.Pts, s.doc.Qts, s.doc.Date, s.doc.Seq = c.Pts, c.Qts, c.Date, c.Seq
	s.doc.Channels = c.Channels
	if s.doc.Channels == nil {
		s.doc.Channels = map[string]int{}
	}
	s.checkpointDirty = true
	return nil
}

// RunCheckpoints 在服务期间定期导出兼容旧版的快照。Open 不启动它，--check 因而不会写盘；
// 即使最后一次游标落盘之后没有新更新，或刚从异常退出恢复，也会在原截止时间导出。
func (s *State) RunCheckpoints(ctx context.Context) {
	retry := false
	for {
		delay := checkpointInterval
		if retry {
			delay = time.Second
		} else {
			s.mu.Lock()
			if s.checkpointDirty {
				delay = max(0, time.Until(s.lastCheckpoint.Add(checkpointInterval)))
			}
			s.mu.Unlock()
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}
		err := s.Checkpoint()
		retry = err != nil
		if err != nil && s.OnFlushError != nil {
			s.OnFlushError(err)
		}
	}
}
