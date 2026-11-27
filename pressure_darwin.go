//go:build darwin

package main

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// memoryPressureLevel returns macOS kern.memorystatus_vm_pressure_level:
// 1 = NORMAL, 2 = WARN, 4 = CRITICAL.
func memoryPressureLevel() int {
	if v := strings.TrimSpace(os.Getenv("SERENA_TEST_PRESSURE_LEVEL")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	v, err := syscall.SysctlUint32("kern.memorystatus_vm_pressure_level")
	if err != nil || v == 0 {
		return 1
	}
	return int(v)
}
