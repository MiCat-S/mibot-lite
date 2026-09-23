package tgstate

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gotd/td/telegram/updates"
)

func TestStateSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.json")
	ctx := context.Background()
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetState(ctx, 7, updates.State{Pts: 10, Qts: 2, Date: 300, Seq: 4}); err != nil {
		t.Fatal(err)
	}
	if err := state.SetChannelPts(ctx, 7, 99, 55); err != nil {
		t.Fatal(err)
	}
	if err := state.SetChannelAccessHash(ctx, 7, 99, 1234); err != nil {
		t.Fatal(err)
	}
	if err := state.SetUserAccessHash(ctx, 7, 5, 6789); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	value, found, err := reopened.GetState(ctx, 7)
	if err != nil || !found || value.Pts != 10 || value.Seq != 4 {
		t.Fatalf("state %+v found=%v err=%v", value, found, err)
	}
	if pts, found, _ := reopened.GetChannelPts(ctx, 7, 99); !found || pts != 55 {
		t.Fatalf("channel pts %d found=%v", pts, found)
	}
	// 没有 access hash，重启后的进程就没法给之后没人说过话的对话发消息，
	// 收藏夹以外的重启回执就是这样坏掉的。
	if hash, found, _ := reopened.GetChannelAccessHash(ctx, 7, 99); !found || hash != 1234 {
		t.Fatalf("channel hash %d found=%v", hash, found)
	}
	if hash, found, _ := reopened.GetUserAccessHash(ctx, 7, 5); !found || hash != 6789 {
		t.Fatalf("user hash %d found=%v", hash, found)
	}
}

// gotd 的约定：还没有任何状态时设置单个字段，必须报告出来，
// 这样管理器才能分清「从没同步过」和「同步到了零」。
func TestSetFieldWithoutStateFails(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "updates.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if err := state.SetPts(context.Background(), 7, 5); err == nil {
		t.Fatal("expected an error when no state exists")
	}
	if _, found, err := state.GetState(context.Background(), 7); found || err != nil {
		t.Fatalf("found=%v err=%v", found, err)
	}
}

// SetState 会清空各频道的位置：它们是相对上一次同步记下的，
// 当成当前位置来读会漏掉更新。
func TestSetStateResetsChannels(t *testing.T) {
	ctx := context.Background()
	state, err := Open(filepath.Join(t.TempDir(), "updates.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	_ = state.SetState(ctx, 1, updates.State{Pts: 1})
	_ = state.SetChannelPts(ctx, 1, 42, 7)
	_ = state.SetState(ctx, 1, updates.State{Pts: 2})
	if _, found, _ := state.GetChannelPts(ctx, 1, 42); found {
		t.Fatal("channel positions should be cleared with the account state")
	}
}

func TestForEachChannelsIsOrdered(t *testing.T) {
	ctx := context.Background()
	state, err := Open(filepath.Join(t.TempDir(), "updates.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	_ = state.SetState(ctx, 1, updates.State{})
	for _, id := range []int64{30, 10, 20} {
		_ = state.SetChannelPts(ctx, 1, id, int(id))
	}
	var seen []int64
	if err := state.ForEachChannels(ctx, 1, func(_ context.Context, channelID int64, pts int) error {
		seen = append(seen, channelID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || seen[0] != 10 || seen[2] != 30 {
		t.Fatalf("visited %v, want ascending order", seen)
	}
}
