//go:build linux

package sysinfo

import "syscall"

func kernelRelease() (string, error) {
	var uname syscall.Utsname
	if err := syscall.Uname(&uname); err != nil {
		return "", err
	}
	buffer := make([]byte, 0, len(uname.Release))
	for _, value := range uname.Release {
		if value == 0 {
			break
		}
		buffer = append(buffer, byte(value))
	}
	return string(buffer), nil
}
