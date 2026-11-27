//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
)

// memoryPressureLevel reads Linux /proc/meminfo and maps available RAM ratio to:
// 1 = NORMAL, 2 = WARN, 4 = CRITICAL.
func memoryPressureLevel() int {
	if v := strings.TrimSpace(os.Getenv("SERENA_TEST_PRESSURE_LEVEL")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 1
	}
	return parseLinuxMeminfoPressure(string(raw))
}
