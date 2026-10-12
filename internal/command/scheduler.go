package command

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

const (
	// 命令池：最多 16 个同时跑，再多 64 个排队。
	commandWorkers = 16
	commandQueue   = 64
	// 短后台任务池：最多 4 个同时跑，再多 16 个排队。
	taskWorkers = 4
	taskQueue   = 16
)

type scheduledWork struct {
	ctx context.Context
	run func(context.Context)
}

// scheduler 是按需启动的有界工作池：最多 limit 个 worker，队列空了 worker 就退出，
// 闲着时不留常驻协程。提交从不阻塞更新回调，排队中的任务也不占协程；队列满了直接拒绝。
type scheduler struct {
	mu      sync.Mutex
	queue   chan scheduledWork
	limit   int
	workers int
	running int
	// idle 在 worker 全部退出时关闭，有新任务时换一个新的。
	idle   chan struct{}
	life   context.Context
	logger *slog.Logger
}

func newScheduler(life context.Context, logger *slog.Logger, workers, queued int) *scheduler {
	idle := make(chan struct{})
	close(idle)
	return &scheduler{queue: make(chan scheduledWork, queued), limit: workers, idle: idle, life: life, logger: logger}
}

func (s *scheduler) submit(ctx context.Context, run func(context.Context)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.life.Err() != nil || ctx.Err() != nil {
		return false
	}
	select {
	case s.queue <- scheduledWork{ctx: ctx, run: run}:
	default:
		return false
	}
	if s.workers == 0 {
		s.idle = make(chan struct{})
	}
	if s.workers < s.limit {
		s.workers++
		go s.work()
	}
	return true
}

func (s *scheduler) work() {
	for {
		s.mu.Lock()
		var task scheduledWork
		select {
		case task = <-s.queue:
			s.running++
		default:
			s.workers--
			if s.workers == 0 {
				close(s.idle)
			}
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		s.execute(task)
		s.mu.Lock()
		s.running--
		s.mu.Unlock()
	}
}

func (s *scheduler) execute(task scheduledWork) {
	if s.life.Err() != nil || task.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(task.ctx)
	stop := context.AfterFunc(s.life, cancel)
	defer cancel()
	defer stop()
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logger.Error("worker.panic", slog.Any("panic", recovered), slog.String("stack", string(debug.Stack())))
		}
	}()
	if ctx.Err() == nil && s.life.Err() == nil {
		task.run(ctx)
	}
}

func (s *scheduler) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *scheduler) wait(timeout time.Duration) bool {
	s.mu.Lock()
	idle := s.idle
	s.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-idle:
		return true
	case <-timer.C:
		return false
	}
}
