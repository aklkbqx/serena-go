//go:build !darwin && !linux

package main

import (
	"os"
	"strconv"
	"strings"
)

func memoryPressureLevel() int {
	if v := strings.TrimSpace(os.Getenv("SERENA_TEST_PRESSURE_LEVEL")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 1
}
