//go:build !linux && !darwin

package sysinfo

// diskUsage 在这个平台上读不到，返回未知。
func diskUsage(string) Capacity { return Capacity{} }
