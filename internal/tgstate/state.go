// Package tgstate 保存 gotd 的更新引擎跨重启需要的东西：账号的更新位置、
// 每个频道的 pts，以及账号见过的所有对象的 access hash。全放在一个 JSON
// 文件里，每秒最多写一次。这样重启后能从停下的地方接着收更新；某个对话
// 在那之后即使再没人说话，也还能编辑其中的消息。
package tgstate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gotd/td/telegram/updates"
)

var errNoState = errors.New("update state does not exist")

type document struct {
	UserID   int64            `json:"userId"`
	Pts      int              `json:"pts"`
	Qts      int              `json:"qts"`
	Date     int              `json:"date"`
	Seq      int              `json:"seq"`
	HasState bool             `json:"hasState"`
	Channels map[string]int   `json:"channels"`
	Chans    map[string]int64 `json:"channelHashes"`
	Users    map[string]int64 `json:"userHashes"`
}

// State 基于一个 JSON 文件实现 updates.StateStorage、
// updates.ChannelAccessHasher 和 updates.UserAccessHasher。
type State struct {
	path  string
	mu    sync.Mutex
	doc   document
	dirty bool
	timer *time.Timer
}

// Open 加载文件；文件不存在就从空状态开始。
func Open(path string) (*State, error) {
	state := &State{path: path, doc: document{Channels: map[string]int{}, Chans: map[string]int64{}, Users: map[string]int64{}}}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &state.doc); err != nil {
			return nil, err
		}
		if state.doc.Channels == nil {
			state.doc.Channels = map[string]int{}
		}
		if state.doc.Chans == nil {
			state.doc.Chans = map[string]int64{}
		}
		if state.doc.Users == nil {
			state.doc.Users = map[string]int64{}
		}
	}
	return state, nil
}

// Close 把尚未写入的改动写到磁盘。
func (s *State) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
	}
	return s.flushLocked()
}

// Flush 立即写入尚未写入的改动。
func (s *State) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *State) markDirty() {
	s.dirty = true
	if s.timer == nil {
		s.timer = time.AfterFunc(time.Second, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			_ = s.flushLocked()
		})
	} else {
		s.timer.Reset(time.Second)
	}
}

func (s *State) flushLocked() error {
	if !s.dirty {
		return nil
	}
	encoded, err := json.Marshal(s.doc)
	if err != nil {
		return err
	}
	temporary := s.path + ".tmp"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, s.path); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

func key(id int64) string { return strconv.FormatInt(id, 10) }

func (s *State) GetState(ctx context.Context, userID int64) (updates.State, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.doc.HasState || s.doc.UserID != userID {
		return updates.State{}, false, nil
	}
	return updates.State{Pts: s.doc.Pts, Qts: s.doc.Qts, Date: s.doc.Date, Seq: s.doc.Seq}, true, nil
}

func (s *State) SetState(ctx context.Context, userID int64, state updates.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc.UserID, s.doc.HasState = userID, true
	s.doc.Pts, s.doc.Qts, s.doc.Date, s.doc.Seq = state.Pts, state.Qts, state.Date, state.Seq
	s.doc.Channels = map[string]int{}
	s.markDirty()
	return nil
}

func (s *State) setField(userID int64, set func()) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.doc.HasState || s.doc.UserID != userID {
		return errNoState
	}
	set()
	s.markDirty()
	return nil
}

func (s *State) SetPts(ctx context.Context, userID int64, pts int) error {
	return s.setField(userID, func() { s.doc.Pts = pts })
}
func (s *State) SetQts(ctx context.Context, userID int64, qts int) error {
	return s.setField(userID, func() { s.doc.Qts = qts })
}
func (s *State) SetDate(ctx context.Context, userID int64, date int) error {
	return s.setField(userID, func() { s.doc.Date = date })
}
func (s *State) SetSeq(ctx context.Context, userID int64, seq int) error {
	return s.setField(userID, func() { s.doc.Seq = seq })
}
func (s *State) SetDateSeq(ctx context.Context, userID int64, date, seq int) error {
	return s.setField(userID, func() { s.doc.Date, s.doc.Seq = date, seq })
}

func (s *State) GetChannelPts(ctx context.Context, userID, channelID int64) (int, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.doc.UserID != userID {
		return 0, false, nil
	}
	pts, ok := s.doc.Channels[key(channelID)]
	return pts, ok, nil
}

func (s *State) SetChannelPts(ctx context.Context, userID, channelID int64, pts int) error {
	return s.setField(userID, func() { s.doc.Channels[key(channelID)] = pts })
}

func (s *State) ForEachChannels(ctx context.Context, userID int64, visit func(ctx context.Context, channelID int64, pts int) error) error {
	s.mu.Lock()
	type entry struct {
		id  int64
		pts int
	}
	var entries []entry
	if s.doc.UserID == userID {
		for id, pts := range s.doc.Channels {
			parsed, err := strconv.ParseInt(id, 10, 64)
			if err == nil {
				entries = append(entries, entry{parsed, pts})
			}
		}
	}
	s.mu.Unlock()
	sort.Slice(entries, func(a, b int) bool { return entries[a].id < entries[b].id })
	for _, item := range entries {
		if err := visit(ctx, item.id, item.pts); err != nil {
			return err
		}
	}
	return nil
}

func (s *State) SetChannelAccessHash(ctx context.Context, userID, channelID, accessHash int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.doc.Chans[key(channelID)] != accessHash {
		s.doc.Chans[key(channelID)] = accessHash
		s.markDirty()
	}
	return nil
}

func (s *State) GetChannelAccessHash(ctx context.Context, userID, channelID int64) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash, ok := s.doc.Chans[key(channelID)]
	return hash, ok, nil
}

func (s *State) SetUserAccessHash(ctx context.Context, userID, targetUserID, accessHash int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.doc.Users[key(targetUserID)] != accessHash {
		s.doc.Users[key(targetUserID)] = accessHash
		s.markDirty()
	}
	return nil
}

func (s *State) GetUserAccessHash(ctx context.Context, userID, targetUserID int64) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash, ok := s.doc.Users[key(targetUserID)]
	return hash, ok, nil
}

// Dir 返回文件所在的目录。
func (s *State) Dir() string { return filepath.Dir(s.path) }
