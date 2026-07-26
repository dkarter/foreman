package main

import (
	"bufio"
	"context"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type systemMetrics struct {
	CPU           *float64 `json:"cpu"`
	RAM           *float64 `json:"ram"`
	RAMUsedBytes  *uint64  `json:"ramUsedBytes"`
	RAMTotalBytes *uint64  `json:"ramTotalBytes"`
}

type resourceMetrics struct {
	Host             systemMetrics `json:"host"`
	Foreman          systemMetrics `json:"foreman"`
	ForemanConnected bool          `json:"foremanConnected"`
}

var hostMemoryTotal = sync.OnceValue(func() *uint64 {
	output, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return nil
	}
	total, err := strconv.ParseUint(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return nil
	}
	return uint64Value(total)
})

func (a *app) monitorMetrics(ctx context.Context) {
	for {
		a.updateHostMetrics(collectHostMetrics(ctx))
		timer := time.NewTimer(time.Duration(a.pollInterval()) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-a.settingsChanged:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func collectHostMetrics(parent context.Context) systemMetrics {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()

	type commandResult struct {
		output []byte
		err    error
	}
	commands := [][]string{
		{"ps", "-A", "-o", "%cpu="},
		{"memory_pressure", "-Q"},
	}
	results := make([]commandResult, len(commands))
	var wait sync.WaitGroup
	for i, command := range commands {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results[i].output, results[i].err = exec.CommandContext(ctx, command[0], command[1:]...).Output()
		}()
	}
	wait.Wait()

	var metrics systemMetrics
	if cpu, ok := parseCPUList(string(results[0].output)); results[0].err == nil && ok {
		metrics.CPU = floatValue(cpu / float64(runtime.NumCPU()))
	}
	if ram, ok := parseMemoryPressure(string(results[1].output)); results[1].err == nil && ok {
		metrics.RAM = floatValue(ram)
	}
	if total := hostMemoryTotal(); total != nil {
		metrics.RAMTotalBytes = total
		if metrics.RAM != nil {
			metrics.RAMUsedBytes = uint64Value(uint64(float64(*total) * *metrics.RAM / 100))
		}
	}
	return metrics
}

func floatValue(value float64) *float64 {
	return &value
}

func uint64Value(value uint64) *uint64 {
	return &value
}

func parseCPUList(output string) (float64, bool) {
	var total float64
	parsed := false
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		value, err := strconv.ParseFloat(strings.TrimSpace(scanner.Text()), 64)
		if err != nil {
			continue
		}
		total += value
		parsed = true
	}
	return total, parsed
}

func parseMemoryPressure(output string) (float64, bool) {
	const marker = "System-wide memory free percentage:"
	for line := range strings.SplitSeq(output, "\n") {
		if value, found := strings.CutPrefix(strings.TrimSpace(line), marker); found {
			free, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(value), "%"), 64)
			if err == nil && free >= 0 && free <= 100 {
				return 100 - free, true
			}
		}
	}
	return 0, false
}
