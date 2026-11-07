package tgstate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gotd/td/telegram/updates"
)

func readDocument(t *testing.T, path string) document {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d document
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func openState(t *testing.T, path string) *State {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !s.closed {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	return s
}

// 模拟进程中断：停掉计时器，不执行 Close 的兼容快照写入。
func crashState(s *State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

func TestCursorFlushRecoversWithoutClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.json")
	ctx := context.Background()
	s := openState(t, path)
	if err := s.SetState(ctx, 7, updates.State{Pts: 10}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserAccessHash(ctx, 7, 42, 99); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := s.SetPts(ctx, 7, 20); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChannelPts(ctx, 7, 88, 30); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("cursor flush rewrote full hash snapshot")
	}
	crashState(s)
	r := openState(t, path)
	value, found, err := r.GetState(ctx, 7)
	if err != nil || !found || value.Pts != 20 {
		t.Fatalf("cursor recovery: %+v %v %v", value, found, err)
	}
	if pts, ok, _ := r.GetChannelPts(ctx, 7, 88); !ok || pts != 30 {
		t.Fatalf("channel recovery: %d %v", pts, ok)
	}
	if hash, ok, _ := r.GetUserAccessHash(ctx, 7, 42); !ok || hash != 99 {
		t.Fatalf("hash recovery: %d %v", hash, ok)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if d := readDocument(t, path); d.Pts != 20 || d.Channels["88"] != 30 {
		t.Fatalf("Close did not export legacy state: %+v", d)
	}
}

func TestCheckpointIgnoresStaleCursorAndOldRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.json")
	ctx := context.Background()
	s := openState(t, path)
	_ = s.SetState(ctx, 7, updates.State{Pts: 10})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = s.SetPts(ctx, 7, 20)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	// 新 hash 必须连游标一起写进旧版能读的快照。
	_ = s.SetUserAccessHash(ctx, 7, 42, 99)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if d := readDocument(t, path); d.Pts != 20 || d.Users["42"] != 99 {
		t.Fatalf("legacy hash checkpoint: %+v", d)
	}
	r := openState(t, path)
	if value, _, _ := r.GetState(ctx, 7); value.Pts != 20 {
		t.Fatal("stale sidecar replaced checkpoint")
	}
	// 旧版忽略 generation，重写后不带它；重新升级不得恢复新版以前的游标。
	d := readDocument(t, path)
	d.Generation, d.Pts = 0, 35
	raw, _ := json.Marshal(d)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	legacy := openState(t, path)
	if value, _, _ := legacy.GetState(ctx, 7); value.Pts != 35 {
		t.Fatal("old-version state overwritten")
	}
	_ = legacy.SetPts(ctx, 7, 40)
	if err := legacy.Flush(); err != nil {
		t.Fatal(err)
	}
	// 重新升级后的第一份快照又用代号1，不能撞上原来代号1的游标。
	again := openState(t, path)
	if value, _, _ := again.GetState(ctx, 7); value.Pts != 40 {
		t.Fatal("reused generation resurrected old cursor")
	}
}

func TestPeriodicAndExplicitCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.json")
	ctx := context.Background()
	s := openState(t, path)
	_ = s.SetState(ctx, 7, updates.State{Pts: 1})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = s.SetPts(ctx, 7, 2)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if d := readDocument(t, path); d.Pts != 2 {
		t.Fatal("explicit checkpoint not exported")
	}
	s.lastCheckpoint = time.Now().Add(-checkpointInterval)
	_ = s.SetPts(ctx, 7, 3)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if d := readDocument(t, path); d.Pts != 3 {
		t.Fatal("periodic checkpoint not exported")
	}
}

func TestOpenDoesNotWriteState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.json")
	s := openState(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("empty read-only open created file")
	}
	if err := os.WriteFile(path, []byte(`{"userId":7,"pts":1,"hasState":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	legacy := openState(t, path)
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("legacy inspection rewrote state")
	}
}

// 193538 个 hash 对应生产表的规模，仅比较五次持久化的实际文件字节，不施加持续负载。
func TestCursorWriteVolume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.json")
	ctx := context.Background()
	d := document{UserID: 7, HasState: true, Users: map[string]int64{}, Chans: map[string]int64{}, Channels: map[string]int{}}
	for i := 1; i <= 193538; i++ {
		d.Users[strconv.Itoa(1000000000+i)] = 1234567890123456789
	}
	for i := 1; i <= 200; i++ {
		d.Channels[strconv.Itoa(i)] = i
	}
	baseline, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, baseline, 0600); err != nil {
		t.Fatal(err)
	}
	s := openState(t, path)
	_ = s.SetPts(ctx, 7, 1)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	full, _ := os.ReadFile(path)
	var smallBytes int
	for i := 2; i <= 6; i++ {
		_ = s.SetPts(ctx, 7, i)
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		cursor, err := os.ReadFile(path + ".cursor")
		if err != nil {
			t.Fatal(err)
		}
		smallBytes += len(cursor)
	}
	unchanged, _ := os.ReadFile(path)
	if string(full) != string(unchanged) {
		t.Fatal("steady cursor traffic rewrote hashes")
	}
	oldBytes := 5 * len(baseline)
	t.Logf("5 steady cursor flushes,193538 hashes: old=%d bytes new=%d bytes reduction=%.3f%% (migration checkpoint excluded)", oldBytes, smallBytes, 100*(1-float64(smallBytes)/float64(oldBytes)))
	if smallBytes >= oldBytes/100 {
		t.Fatal("cursor flush did not reduce writes by99%")
	}
}

func TestCursorFlushFailureCanRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.json")
	ctx := context.Background()
	s := openState(t, path)
	_ = s.SetState(ctx, 7, updates.State{Pts: 1})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".cursor", 0700); err != nil {
		t.Fatal(err)
	}
	_ = s.SetPts(ctx, 7, 2)
	if err := s.Flush(); err == nil {
		t.Fatal("cursor write failure hidden")
	}
	if err := os.Remove(path + ".cursor"); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	r := openState(t, path)
	if value, _, _ := r.GetState(ctx, 7); value.Pts != 2 {
		t.Fatal("retry lost cursor")
	}
}

func TestRestartKeepsCheckpointDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.json")
	ctx := context.Background()
	s := openState(t, path)
	_ = s.SetState(ctx, 7, updates.State{Pts: 1})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = s.SetPts(ctx, 7, 2)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-checkpointInterval - time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	r := openState(t, path)
	_ = r.SetPts(ctx, 7, 3)
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	if d := readDocument(t, path); d.Pts != 3 {
		t.Fatal("restart extended checkpoint deadline")
	}
	// r 是模拟重启后的进程，旧实例不能再 Close 写回。
	crashState(s)
}

func TestRuntimeCheckpointsRecoveredIdleCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.json")
	ctx := context.Background()
	s := openState(t, path)
	_ = s.SetState(ctx, 7, updates.State{Pts: 1})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = s.SetPts(ctx, 7, 2)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	crashState(s)
	old := time.Now().Add(-checkpointInterval - time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	r := openState(t, path)
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); r.RunCheckpoints(run) }()
	deadline := time.Now().Add(time.Second)
	for readDocument(t, path).Pts != 2 {
		if time.Now().After(deadline) {
			t.Fatal("idle recovered cursor never checkpointed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("checkpoint worker did not stop")
	}
}
