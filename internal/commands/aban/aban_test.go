package aban

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	for input, want := range map[string]time.Duration{
		"60s": time.Minute, "5m": 5 * time.Minute, "1h": time.Hour, "2d": 48 * time.Hour,
		"": 0, "forever": 0, "0m": 0, "-3h": 0, "90": 0,
		// 原版 parseInt 加「含有哪个字母」：单位可以写全，多段只认第一个数和最大的单位。
		"5min": 5 * time.Minute, "1day": 24 * time.Hour, "2hours": 2 * time.Hour, "10sec": 10 * time.Second,
		"1h30m": time.Hour, " 7D": 7 * 24 * time.Hour, "+5m": 5 * time.Minute, "m5": 0,
		"99999999999999999999d": 0, "999999999999d": 0,
	} {
		if got := parseDuration(input); got != want {
			t.Errorf("parseDuration(%q) = %v, want %v", input, got, want)
		}
	}
	if got := formatDuration(0); got != "永久" {
		t.Fatalf("formatDuration(0) = %q", got)
	}
	if got := formatDuration(90 * time.Minute); got != "1h" {
		t.Fatalf("formatDuration(90m) = %q", got)
	}
}

func TestIsNumericID(t *testing.T) {
	for input, want := range map[string]bool{"42": true, "-1001234": true, "-": false, "": false, "@42": false, "4x2": false, "+42": false} {
		if got := isNumericID(input); got != want {
			t.Errorf("isNumericID(%q) = %v, want %v", input, got, want)
		}
	}
}
