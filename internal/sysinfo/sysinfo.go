// Package sysinfo 为 .memory、.status 和 .sysinfo 提供进程和主机的
// 资源占用情况。
package sysinfo

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Memory 是进程内存的快照，单位为字节。
type Memory struct {
	RSS        uint64
	HeapAlloc  uint64
	HeapSys    uint64
	Sys        uint64
	Goroutines int
}

// Read 采样当前的内存占用。
func Read() Memory {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return Memory{RSS: residentBytes(), HeapAlloc: stats.HeapAlloc, HeapSys: stats.HeapSys, Sys: stats.Sys, Goroutines: runtime.NumGoroutine()}
}

func residentBytes() uint64 {
	if runtime.GOOS == "linux" {
		raw, err := os.ReadFile("/proc/self/statm")
		if err == nil {
			fields := strings.Fields(string(raw))
			if len(fields) >= 2 {
				if pages, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
					return pages * uint64(os.Getpagesize())
				}
			}
		}
		return 0
	}
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0
	}
	kilobytes, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0
	}
	return kilobytes * 1024
}

// Megabytes 把字节数换算成用于显示的 MB 数。
func Megabytes(value uint64) float64 { return float64(value) / (1 << 20) }

// Host 是 .sysinfo 报告的机器信息。
type Host struct {
	Hostname      string
	Platform      string
	KernelRelease string
	Uptime        time.Duration
	LoadAverage   [3]float64
	CPUs          int
	TotalMemory   uint64
	FreeMemory    uint64
}

// ReadHost 读取主机的资源状态；哪个来源读取失败，对应字段就保持零值。
func ReadHost() Host {
	host := Host{Platform: runtime.GOOS + " " + runtime.GOARCH, CPUs: runtime.NumCPU()}
	if name, err := os.Hostname(); err == nil {
		host.Hostname = name
	}
	if release, err := kernelRelease(); err == nil {
		host.KernelRelease = release
	}
	if seconds, err := readUptime("/proc/uptime"); err == nil {
		host.Uptime = time.Duration(seconds * float64(time.Second))
	}
	if load, err := readLoadAverage("/proc/loadavg"); err == nil {
		host.LoadAverage = load
	}
	if total, free, err := readMemInfo("/proc/meminfo"); err == nil {
		host.TotalMemory, host.FreeMemory = total, free
	}
	return host
}

func readUptime(path string) (float64, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(content))
	if len(fields) < 1 {
		return 0, fmt.Errorf("unexpected %s contents", path)
	}
	return strconv.ParseFloat(fields[0], 64)
}

func readLoadAverage(path string) ([3]float64, error) {
	var result [3]float64
	content, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	fields := strings.Fields(string(content))
	if len(fields) < 3 {
		return result, fmt.Errorf("unexpected %s contents", path)
	}
	for index := 0; index < 3; index++ {
		value, err := strconv.ParseFloat(fields[index], 64)
		if err != nil {
			return result, err
		}
		result[index] = value
	}
	return result, nil
}

func readMemInfo(path string) (total, free uint64, err error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var key string
		var kb uint64
		if _, scanErr := fmt.Sscanf(scanner.Text(), "%s %d", &key, &kb); scanErr != nil {
			continue
		}
		switch key {
		case "MemTotal:":
			total = kb * 1024
		case "MemAvailable:":
			free = kb * 1024
		}
	}
	return total, free, scanner.Err()
}
