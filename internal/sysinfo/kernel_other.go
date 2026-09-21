//go:build !linux

package sysinfo

import (
	"os/exec"
	"strings"
)

func kernelRelease() (string, error) {
	out, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
