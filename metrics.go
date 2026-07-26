package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type resourceMetrics struct {
	HostCPU       *float64 `json:"hostCpu"`
	HostRAM       *float64 `json:"hostRam"`
	ForemanCPU    *float64 `json:"foremanCpu"`
	ForemanRAMMiB *float64 `json:"foremanRamMiB"`
}

func (a *app) monitorMetrics(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		a.updateMetrics(collectMetrics(ctx))
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func collectMetrics(parent context.Context) resourceMetrics {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()

	type commandResult struct {
		output []byte
		err    error
	}
	commands := [][]string{
		{"ps", "-A", "-o", "%cpu="},
		{"memory_pressure", "-Q"},
		{"ps", "-p", strconv.Itoa(os.Getpid()), "-o", "%cpu=,rss="},
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

	var metrics resourceMetrics
	if cpu, ok := parseCPUList(string(results[0].output)); results[0].err == nil && ok {
		metrics.HostCPU = metricValue(cpu / float64(runtime.NumCPU()))
	}
	if ram, ok := parseMemoryPressure(string(results[1].output)); results[1].err == nil && ok {
		metrics.HostRAM = metricValue(ram)
	}
	if cpu, ram, ok := parseProcessMetrics(string(results[2].output)); results[2].err == nil && ok {
		metrics.ForemanCPU = metricValue(cpu)
		metrics.ForemanRAMMiB = metricValue(ram)
	}
	return metrics
}

func metricValue(value float64) *float64 {
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

func parseProcessMetrics(output string) (cpu float64, ramMiB float64, ok bool) {
	fields := strings.Fields(output)
	if len(fields) != 2 {
		return 0, 0, false
	}
	cpu, cpuErr := strconv.ParseFloat(fields[0], 64)
	rssKiB, ramErr := strconv.ParseFloat(fields[1], 64)
	return cpu, rssKiB / 1024, cpuErr == nil && ramErr == nil
}
