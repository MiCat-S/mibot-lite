package da

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
)

// .da stop 赶在任务开始前取消时，任务根本不会执行，群的名额也必须还回来，
// 否则这个群之后再也开不了 .da，攒满 4 个后别的群也开不了。
func TestCanceledBeforeRunReleasesSlot(t *testing.T) {
	r := command.New([]string{"."}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer r.Abort()
	// 先把长任务池占满，.da 的任务只能排队，好在它开始之前取消。
	blocker := make(chan struct{})
	started := make(chan struct{}, command.LongTasks)
	for range command.LongTasks {
		r.LongRunning(r.Lifecycle(), func(context.Context) { started <- struct{}{}; <-blocker }, nil)
		<-started
	}
	s := &daService{a: &app.App{Registry: r}, active: map[string]*daSlot{}}
	inv := &command.Invocation{Message: &bot.Message{ChatID: "-1001", ID: 1}}
	if ok, err := s.start(inv); !ok || err != nil {
		t.Fatalf("start: %v %v", ok, err)
	}
	s.mu.Lock()
	s.active["-1001"].cancel()
	s.mu.Unlock()
	close(blocker)
	if !r.Wait(time.Second) {
		t.Fatal("池没有排空")
	}
	s.mu.Lock()
	left := len(s.active)
	s.mu.Unlock()
	if left != 0 {
		t.Fatalf("还占着 %d 个名额", left)
	}
}

// 同时在删的群满了，再开要直接说明原因，而不是悄悄排队或者什么都不说。
func TestStartRefusesBeyondLimit(t *testing.T) {
	r := command.New([]string{"."}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer r.Abort()
	s := &daService{a: &app.App{Registry: r}, active: map[string]*daSlot{}}
	for index := range command.LongTasks {
		s.active[string(rune('a'+index))] = &daSlot{cancel: func() {}}
	}
	ok, err := s.start(&command.Invocation{Message: &bot.Message{ChatID: "-1009", ID: 1}})
	if ok || err == nil {
		t.Fatalf("超出上限仍启动了：%v %v", ok, err)
	}
}
