package sysinfo

import (
	"testing"
	"time"
)

// 同一个时长、同一个大小各处只有一种写法：.status 的卡片和文字、.sysinfo、.speedtest、.bf 都用这两个函数。
func TestFormatUptimeAndBytes(t *testing.T) {
	for value, want := range map[time.Duration]string{
		31*time.Minute + 9*time.Second:                               "00:31:09",
		2*24*time.Hour + 3*time.Hour + 4*time.Minute + 5*time.Second: "2天 03:04:05",
		-time.Second: "--",
	} {
		if got := FormatUptime(value); got != want {
			t.Errorf("FormatUptime(%v) = %q, want %q", value, got, want)
		}
	}
	for size, want := range map[uint64]string{12: "12 B", 5 << 10: "5.0 KB", 298520000: "284.7 MB", 5 << 30: "5.00 GB"} {
		if got := FormatBytes(size); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", size, got, want)
		}
	}
}
