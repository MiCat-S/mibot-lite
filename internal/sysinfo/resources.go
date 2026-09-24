package sysinfo

import (
	"bufio"
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Capacity 是一种资源的用量；Known 为假表示本机读不到。
type Capacity struct {
	Used, Total uint64
	Known       bool
}

// Percent 是用量占总量的百分比，读不到时第二个返回值为假。
func (c Capacity) Percent() (float64, bool) {
	if !c.Known {
		return 0, false
	}
	if c.Total == 0 {
		return 0, true
	}
	return min(100, float64(c.Used)/float64(c.Total)*100), true
}

// Resources 是 .status 卡片要的资源水位。
type Resources struct {
	// SystemCPU 和 ProcessCPU 是采样窗口内整机和本进程的 CPU 占用（百分比），
	// 本进程的按核数平摊；HasCPU 为假表示读不到 /proc/stat。
	SystemCPU, ProcessCPU float64
	HasCPU                bool
	Memory, Swap, Disk    Capacity
	// OS 是 /etc/os-release 里的 PRETTY_NAME，读不到时是 GOOS。
	OS      string
	Sampled time.Duration
}

// cpuTicks 是 /proc/stat 第一行：空闲（含 iowait）和总的时钟滴答数。
func cpuTicks() (idle, total uint64, ok bool) {
	file, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return 0, 0, false
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	for index, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		total += value
		// 第 4、5 项是 idle 和 iowait。
		if index == 3 || index == 4 {
			idle += value
		}
	}
	return idle, total, true
}

// processCPU 是本进程至今用掉的 CPU 时间（用户态加内核态）。
func processCPU() time.Duration {
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// SampleResources 采样一次资源水位：CPU 要隔 window 读两次才算得出占用，
// 其余读一次。root 所在的文件系统算作「磁盘」。
func SampleResources(ctx context.Context, root string, window time.Duration) Resources {
	started := time.Now()
	idleBefore, totalBefore, cpuOK := cpuTicks()
	processBefore := processCPU()
	timer := time.NewTimer(window)
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
	timer.Stop()
	idleAfter, totalAfter, cpuAfterOK := cpuTicks()
	elapsed := time.Since(started)
	result := Resources{OS: osRelease(), Disk: diskUsage(root)}
	if cpuOK && cpuAfterOK && totalAfter > totalBefore {
		busy := float64((totalAfter-totalBefore)-(idleAfter-idleBefore)) / float64(totalAfter-totalBefore)
		result.SystemCPU, result.HasCPU = max(0, min(100, busy*100)), true
	}
	if elapsed > 0 {
		cores := float64(max(1, runtime.NumCPU()))
		result.ProcessCPU = max(0, min(100, float64(processCPU()-processBefore)/float64(elapsed)/cores*100))
	}
	result.Memory, result.Swap = readMemoryAndSwap("/proc/meminfo")
	result.Sampled = time.Since(started)
	return result
}

// readMemoryAndSwap 读出内存（总量减可用量）和 Swap（总量减空闲量）的用量。
func readMemoryAndSwap(path string) (memory, swap Capacity) {
	content, err := os.ReadFile(path)
	if err != nil {
		return
	}
	values := map[string]uint64{}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			if value, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				values[strings.TrimSuffix(fields[0], ":")] = value * 1024
			}
		}
	}
	if total, ok := values["MemTotal"]; ok {
		available, has := values["MemAvailable"]
		if !has {
			available = values["MemFree"]
		}
		memory = Capacity{Used: total - min(total, available), Total: total, Known: true}
	}
	if total, ok := values["SwapTotal"]; ok {
		swap = Capacity{Used: total - min(total, values["SwapFree"]), Total: total, Known: true}
	}
	return memory, swap
}

// osRelease 是发行版名称，如 "Debian GNU/Linux 13 (trixie)"。
func osRelease() string {
	content, err := os.ReadFile("/etc/os-release")
	if err == nil {
		for _, line := range strings.Split(string(content), "\n") {
			if value, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
				if value = strings.Trim(strings.TrimSpace(value), `"'`); value != "" {
					return value
				}
			}
		}
	}
	return runtime.GOOS
}
