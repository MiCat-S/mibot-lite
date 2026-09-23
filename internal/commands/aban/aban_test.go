package aban

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	for input, want := range map[string]time.Duration{
		"60s": time.Minute, "5m": 5 * time.Minute, "1h": time.Hour, "2d": 48 * time.Hour,
		"": 0, "forever": 0, "0m": 0, "-3h": 0,
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
