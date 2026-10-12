package command

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/bot"
)

func TestSchedulerBoundsExecutionAndQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newScheduler(ctx, slog.Default(), 2, 3)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var running, peak atomic.Int32
	work := func(context.Context) {
		n := running.Add(1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		started <- struct{}{}
		<-release
		running.Add(-1)
	}
	for range 2 {
		if !s.submit(ctx, work) {
			t.Fatal("initial work rejected")
		}
		<-started
	}
	for range 3 {
		if !s.submit(ctx, func(context.Context) {}) {
			t.Fatal("queue rejected too early")
		}
	}
	for range 10000 {
		if s.submit(ctx, work) {
			t.Fatal("unbounded queue admitted work")
		}
	}
	if s.wait(time.Millisecond) {
		t.Fatal("reported idle with work running")
	}
	close(release)
	if !s.wait(time.Second) || peak.Load() != 2 {
		t.Fatalf("did not drain or exceeded workers: peak=%d", peak.Load())
	}
	// 闲下来的 worker 已经退出，新的一批任务仍要能跑。
	done := make(chan struct{})
	if !s.submit(ctx, func(context.Context) { close(done) }) {
		t.Fatal("could not restart workers")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second batch hung")
	}
}

func TestSchedulerAbortDropsQueuedWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := newScheduler(ctx, slog.Default(), 1, 2)
	started := make(chan struct{})
	s.submit(ctx, func(ctx context.Context) { close(started); <-ctx.Done() })
	<-started
	var calls atomic.Int32
	s.submit(ctx, func(context.Context) { calls.Add(1) })
	cancel()
	if !s.wait(time.Second) || calls.Load() != 0 {
		t.Fatal("queued work ran after abort")
	}
	if s.submit(context.Background(), func(context.Context) {}) {
		t.Fatal("admitted work after abort")
	}
}

func TestRegistryWaitIncludesCommandsSubmittedByBackground(t *testing.T) {
	r := registry()
	defer r.Abort()
	finished := make(chan struct{})
	r.Register(&Command{Name: "nested", Handle: func(context.Context, *Invocation) error { close(finished); return nil }})
	if !r.Background(context.Background(), func(context.Context) {
		r.Dispatch(r.Lifecycle(), nil, &bot.Message{Text: ".nested", ChatID: "1"})
	}) {
		t.Fatal("background rejected")
	}
	if !r.Wait(time.Second) {
		t.Fatal("did not become idle")
	}
	select {
	case <-finished:
	default:
		t.Fatal("Wait returned before nested command")
	}
}

func TestRegistryDispatchAbortWaitConcurrent(t *testing.T) {
	r := registry()
	r.Register(&Command{Name: "fast", Handle: func(context.Context, *Invocation) error { return nil }})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				r.Dispatch(context.Background(), nil, &bot.Message{Text: ".fast", ChatID: "1"})
				r.Wait(time.Millisecond)
			}
		})
	}
	r.Abort()
	wg.Wait()
	if !r.Wait(time.Second) {
		t.Fatal("work lingered")
	}
}

// 长任务占满长任务池时，代发这类短后台任务仍要能马上跑，Abort 后两边都要退干净。
func TestLongTasksDoNotBlockBackground(t *testing.T) {
	r := registry()
	started := make(chan struct{}, LongTasks)
	for range LongTasks {
		if !r.LongRunning(r.Lifecycle(), func(ctx context.Context) { started <- struct{}{}; <-ctx.Done() }, nil) {
			t.Fatal("long task rejected")
		}
	}
	for range LongTasks {
		<-started
	}
	done := make(chan struct{})
	if !r.Background(r.Lifecycle(), func(context.Context) { close(done) }) {
		t.Fatal("background rejected while long pool is full")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("background task waited for long tasks")
	}
	if r.Wait(10 * time.Millisecond) {
		t.Fatal("Wait ignored running long tasks")
	}
	r.Abort()
	if !r.Wait(time.Second) {
		t.Fatal("long tasks lingered after Abort")
	}
}

// 排队期间被取消、没开始跑的任务也要调用 release，否则调用方提交前占下的名额永远还不回来。
func TestSchedulerReleasesCanceledQueuedWork(t *testing.T) {
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newScheduler(life, slog.Default(), 1, 1)
	blocker := make(chan struct{})
	started := make(chan struct{})
	s.submit(life, func(context.Context) { close(started); <-blocker })
	<-started
	taskCtx, cancelTask := context.WithCancel(life)
	var ran, released atomic.Int32
	if !s.admit(taskCtx, func(context.Context) { ran.Add(1) }, func() { released.Add(1) }) {
		t.Fatal("rejected")
	}
	cancelTask()
	close(blocker)
	if !s.wait(time.Second) || ran.Load() != 0 || released.Load() != 1 {
		t.Fatalf("ran=%d released=%d", ran.Load(), released.Load())
	}
}

// 命令池排满时，Submit 要如实报告没有执行，Dispatch 仍按「匹配到命令」返回 true。
func TestSubmitReportsRejectionOnFullQueue(t *testing.T) {
	r := registry()
	defer r.Abort()
	started := make(chan struct{}, commandWorkers)
	release := make(chan struct{})
	r.Register(&Command{Name: "blocked", Handle: func(context.Context, *Invocation) error { started <- struct{}{}; <-release; return nil }},
		&Command{Name: "target", Handle: func(context.Context, *Invocation) error { return nil }})
	for range commandWorkers {
		r.Dispatch(r.Lifecycle(), nil, &bot.Message{Text: ".blocked", ChatID: "1"})
		<-started
	}
	for range commandQueue {
		if got := r.Submit(r.Lifecycle(), nil, &bot.Message{Text: ".target", ChatID: "1"}); got != Queued {
			t.Fatalf("queued submit = %v", got)
		}
	}
	if r.Busy() != commandWorkers+commandQueue {
		t.Fatalf("Busy = %d", r.Busy())
	}
	if got := r.Submit(r.Lifecycle(), nil, &bot.Message{Text: ".target", ChatID: "1"}); got != Rejected {
		t.Fatalf("full queue submit = %v", got)
	}
	if got := r.Submit(r.Lifecycle(), nil, &bot.Message{Text: "hello", ChatID: "1"}); got != NotCommand {
		t.Fatalf("plain text submit = %v", got)
	}
	close(release)
	if !r.Wait(time.Second) || r.Busy() != 0 {
		t.Fatal("did not drain")
	}
}

// 长任务和 Track 记下的工作都算忙，自动更新靠它判断能不能重启。
func TestBusyCountsLongTasksAndTracked(t *testing.T) {
	r := registry()
	started := make(chan struct{})
	r.LongRunning(r.Lifecycle(), func(ctx context.Context) { close(started); <-ctx.Done() }, nil)
	<-started
	if r.Running() != 0 || r.Busy() != 1 {
		t.Fatalf("Running=%d Busy=%d", r.Running(), r.Busy())
	}
	done, _ := r.Track()
	if r.Busy() != 2 {
		t.Fatalf("Busy with tracked = %d", r.Busy())
	}
	done()
	done()
	r.Abort()
	if !r.Wait(time.Second) || r.Busy() != 0 {
		t.Fatalf("Busy after abort = %d", r.Busy())
	}
}

// 延时任务到点后带着服务的 ctx 执行；Abort 之后还没到点的放弃，计数归零，之后也不再接受。
func TestAfterRunsAndStopsOnAbort(t *testing.T) {
	r := registry()
	ran := make(chan error, 1)
	if !r.After(time.Millisecond, func(ctx context.Context) { ran <- ctx.Err() }) {
		t.Fatal("After rejected")
	}
	select {
	case err := <-ran:
		if err != nil {
			t.Fatal("delayed task got a canceled ctx")
		}
	case <-time.After(time.Second):
		t.Fatal("delayed task did not run")
	}
	var late atomic.Int32
	for range 10 {
		r.After(time.Hour, func(context.Context) { late.Add(1) })
	}
	if r.timers.Load() != 10 {
		t.Fatalf("timers = %d", r.timers.Load())
	}
	r.Abort()
	// context.AfterFunc 的回调在 Abort 之后异步执行，计数稍后才归零。
	deadline := time.Now().Add(time.Second)
	for r.timers.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !r.Wait(time.Second) || late.Load() != 0 || r.timers.Load() != 0 {
		t.Fatalf("late=%d timers=%d", late.Load(), r.timers.Load())
	}
	if r.After(0, func(context.Context) { late.Add(1) }) {
		t.Fatal("After accepted work after Abort")
	}
	// 退出后被拒的调用没占名额，也不能把计数减成负数。
	if r.timers.Load() != 0 {
		t.Fatalf("timers after rejected After = %d", r.timers.Load())
	}
}

// 维护闸门关上后，命令、后台任务、长任务和 Track 都要被拒；Resume 后恢复。
func TestQuiesceClosesAdmission(t *testing.T) {
	r := registry()
	defer r.Abort()
	r.Register(&Command{Name: "fast", Handle: func(context.Context, *Invocation) error { return nil }})
	if !r.Quiesce(context.Background(), time.Second) {
		t.Fatal("idle registry did not quiesce")
	}
	if r.LongRunning(r.Lifecycle(), func(context.Context) {}, nil) || r.Background(r.Lifecycle(), func(context.Context) {}) {
		t.Fatal("task admitted during maintenance")
	}
	if got := r.Submit(r.Lifecycle(), nil, &bot.Message{Text: ".fast", ChatID: "1"}); got != Rejected {
		t.Fatalf("command during maintenance = %v", got)
	}
	if _, ok := r.Track(); ok {
		t.Fatal("Track accepted during maintenance")
	}
	r.Resume()
	done, ok := r.Track()
	if !ok || !r.Background(r.Lifecycle(), func(context.Context) {}) {
		t.Fatal("admission not restored by Resume")
	}
	done()
}

// 关闸门之前接受的工作要等它结束；代发这类「后台任务交给命令池」的交接在闸门关着时被拒，
// 不会出现「两边都读到 0、其实还有命令」的情况。
func TestQuiesceWaitsForAcceptedWork(t *testing.T) {
	r := registry()
	defer r.Abort()
	var handed atomic.Int32
	r.Register(&Command{Name: "nested", Handle: func(context.Context, *Invocation) error { handed.Add(1); return nil }})
	release := make(chan struct{})
	started := make(chan struct{})
	r.Background(r.Lifecycle(), func(context.Context) {
		close(started)
		<-release
		r.Submit(r.Lifecycle(), nil, &bot.Message{Text: ".nested", ChatID: "1"})
	})
	<-started
	result := make(chan bool, 1)
	go func() { result <- r.Quiesce(context.Background(), time.Second) }()
	time.Sleep(20 * time.Millisecond)
	close(release)
	if !<-result {
		t.Fatal("did not quiesce after work finished")
	}
	if r.Busy() != 0 || handed.Load() != 0 {
		t.Fatalf("Busy=%d handed=%d", r.Busy(), handed.Load())
	}
}

// 等不到空闲时要重新打开闸门，不能让服务一直拒绝命令。
func TestQuiesceTimeoutReopens(t *testing.T) {
	r := registry()
	done, _ := r.Track()
	if r.Quiesce(context.Background(), 30*time.Millisecond) {
		t.Fatal("quiesced while tracked work was running")
	}
	if !r.Background(r.Lifecycle(), func(context.Context) {}) {
		t.Fatal("gate stayed closed after timeout")
	}
	done()
	r.Abort()
}
