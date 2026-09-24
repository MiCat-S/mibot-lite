//go:build linux || darwin

package sysinfo

import "syscall"

// diskUsage 是 path 所在文件系统的用量。
func diskUsage(path string) Capacity {
	var stat syscall.Statfs_t
	if syscall.Statfs(path, &stat) != nil {
		return Capacity{}
	}
	size := uint64(stat.Bsize)
	total, free := uint64(stat.Blocks)*size, uint64(stat.Bfree)*size
	if total == 0 {
		return Capacity{}
	}
	return Capacity{Used: total - min(total, free), Total: total, Known: true}
}
