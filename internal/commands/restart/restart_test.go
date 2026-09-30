package restart

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// systemctl 被这次重启本身打断（被信号结束、命令的 ctx 被取消）时回执要留着，重启回来才能
// 改那条消息；真的失败（非零退出、超时）才撤掉回执，并且允许再试。
func TestScheduleKeepsReceiptWhenInterrupted(t *testing.T) {
	previous := triggerRestart
	t.Cleanup(func() { triggerRestart = previous })
	newRestarter := func(t *testing.T) *Restarter {
		return &Restarter{a: &app.App{BootID: "old"}, service: "mibot-lite.service",
			store: store.New(filepath.Join(t.TempDir(), "restart-receipt.json"), func() receiptDocument { return receiptDocument{} })}
	}
	killed := func(context.Context, string) error { return exec.Command("sh", "-c", "kill -TERM $$").Run() }
	failed := func(context.Context, string) error { return exec.Command("sh", "-c", "exit 1").Run() }

	for name, c := range map[string]struct {
		trigger  func(context.Context, string) error
		canceled bool
		keep     bool
	}{
		"提交成功":            {trigger: func(context.Context, string) error { return nil }, keep: true},
		"systemctl 被信号结束": {trigger: killed, keep: true},
		"命令的 ctx 被取消":     {trigger: func(ctx context.Context, _ string) error { return ctx.Err() }, canceled: true, keep: true},
		"systemctl 非零退出":  {trigger: failed},
		"systemctl 超时": {trigger: func(context.Context, string) error {
			return fmt.Errorf("%w: %w", errTriggerTimeout, exec.Command("sh", "-c", "kill -KILL $$").Run())
		}},
	} {
		triggerRestart = c.trigger
		r := newRestarter(t)
		ctx, cancel := context.WithCancel(context.Background())
		if c.canceled {
			cancel()
		}
		err := r.Schedule(ctx, "812473405", 309529, "update")
		cancel()
		doc, _ := r.store.Read()
		var failure triggerError
		switch {
		case c.keep && (err != nil || doc.Pending == nil || doc.Pending.MessageID != 309529 || doc.Pending.Kind != "update"):
			t.Errorf("%s：回执应留着，得到 %v %+v", name, err, doc.Pending)
		case !c.keep && (!errors.As(err, &failure) || doc.Pending != nil):
			t.Errorf("%s：应报失败并撤掉回执，得到 %v %+v", name, err, doc.Pending)
		case !c.keep && errors.Is(r.Schedule(context.Background(), "1", 1, "restart"), ErrPending):
			t.Errorf("%s：失败之后应该可以再试", name)
		case c.keep && !errors.Is(r.Schedule(context.Background(), "1", 1, "restart"), ErrPending):
			t.Errorf("%s：重启已经提交，再来一次应回 ErrPending", name)
		}
	}
}
