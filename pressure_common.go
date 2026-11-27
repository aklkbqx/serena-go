package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

func adaptiveLSPPolicy() (maxDaemons int, idleTimeout time.Duration) {
	maxDaemons = 2
	idleTimeout = 180 * time.Second
	if v := strings.TrimSpace(os.Getenv("SERENA_MAX_LSPS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxDaemons = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("SERENA_LSP_IDLE_SECS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			idleTimeout = time.Duration(n) * time.Second
		}
	}
	switch level := memoryPressureLevel(); {
	case level >= 4:
		return 0, 10 * time.Second
	case level >= 2:
		if maxDaemons > 1 {
			maxDaemons = 1
		}
		if idleTimeout > 45*time.Second {
			idleTimeout = 45 * time.Second
		}
		return maxDaemons, idleTimeout
	default:
		return maxDaemons, idleTimeout
	}
}

func memoryPressureName(level int) string {
	switch {
	case level >= 4:
		return "CRITICAL"
	case level >= 2:
		return "WARN"
	default:
		return "NORMAL"
	}
}

// parseLinuxMeminfoPressure derives a macOS-compatible pressure level (1/2/4)
// from Linux /proc/meminfo MemTotal and MemAvailable fields.
func parseLinuxMeminfoPressure(content string) int {
	var totalKB, availKB int64
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			if v, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				totalKB = v
			}
		case "MemAvailable:":
			if v, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				availKB = v
			}
		}
	}
	if totalKB <= 0 || availKB <= 0 {
		return 1
	}
	ratio := float64(availKB) / float64(totalKB)
	switch {
	case ratio < 0.08 || availKB < 512*1024:
		return 4
	case ratio < 0.18 || availKB < 1536*1024:
		return 2
	default:
		return 1
	}
}
