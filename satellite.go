package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type linuxMetricsCollector struct {
	previousIdle  uint64
	previousTotal uint64
}

func (collector *linuxMetricsCollector) collect() systemMetrics {
	var metrics systemMetrics
	if data, err := os.ReadFile("/proc/stat"); err == nil {
		if idle, total, ok := parseProcStat(string(data)); ok {
			if collector.previousTotal > 0 && total > collector.previousTotal {
				totalDelta := total - collector.previousTotal
				idleDelta := idle - collector.previousIdle
				metrics.CPU = floatValue(float64(totalDelta-idleDelta) * 100 / float64(totalDelta))
			}
			collector.previousIdle = idle
			collector.previousTotal = total
		}
	}
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		if used, total, ok := parseProcMeminfo(string(data)); ok {
			metrics.RAMUsedBytes = uint64Value(used)
			metrics.RAMTotalBytes = uint64Value(total)
			metrics.RAM = floatValue(float64(used) * 100 / float64(total))
		}
	}
	return metrics
}

func parseProcStat(output string) (idle uint64, total uint64, ok bool) {
	line, _, found := strings.Cut(output, "\n")
	fields := strings.Fields(line)
	if !found || len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	values := make([]uint64, 0, len(fields)-1)
	for _, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		values = append(values, value)
		total += value
	}
	idle = values[3]
	if len(values) > 4 {
		idle += values[4]
	}
	return idle, total, true
}

func parseProcMeminfo(output string) (used uint64, total uint64, ok bool) {
	values := map[string]uint64{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err == nil {
			values[strings.TrimSuffix(fields[0], ":")] = value * 1024
		}
	}
	total = values["MemTotal"]
	available := values["MemAvailable"]
	if total == 0 || available > total {
		return 0, 0, false
	}
	return total - available, total, true
}

func runSatellite(ctx context.Context, serverURL string) error {
	collector := &linuxMetricsCollector{}
	for ctx.Err() == nil {
		if err := runSatelliteSession(ctx, serverURL, collector); err != nil && ctx.Err() == nil {
			log.Printf("Foreman satellite disconnected: %v", err)
			if !sleep(ctx, 2*time.Second) {
				return ctx.Err()
			}
		}
	}
	return ctx.Err()
}

func runSatelliteSession(ctx context.Context, serverURL string, collector *linuxMetricsCollector) error {
	conn, response, err := websocket.Dial(ctx, serverURL, nil)
	if err != nil {
		if response != nil {
			return fmt.Errorf("satellite connection failed: %s", response.Status)
		}
		return err
	}
	defer conn.CloseNow()

	intervals := make(chan int, 1)
	errors := make(chan error, 1)
	go func() {
		for {
			var message struct {
				Type                string `json:"type"`
				PollIntervalSeconds int    `json:"pollIntervalSeconds"`
			}
			if err := wsjson.Read(ctx, conn, &message); err != nil {
				errors <- err
				return
			}
			if message.Type == "settings" && validPollInterval(message.PollIntervalSeconds) {
				queueLatest(intervals, message.PollIntervalSeconds)
			}
		}
	}()

	interval := defaultPollInterval
	for {
		message := map[string]any{"type": "metrics", "metrics": collector.collect()}
		writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := wsjson.Write(writeCtx, conn, message)
		cancel()
		if err != nil {
			return err
		}
		timer := time.NewTimer(time.Duration(interval) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case err := <-errors:
			timer.Stop()
			return err
		case interval = <-intervals:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func satelliteURL() string {
	if configured := os.Getenv("FOREMAN_SERVER_URL"); configured != "" {
		return resolveLocalHostname(configured)
	}
	return resolveLocalHostname("ws://air.local:4040/satellite")
}

func resolveLocalHostname(serverURL string) string {
	const hostname = "air.local"
	if !strings.Contains(serverURL, hostname) {
		return serverURL
	}
	output, err := exec.Command("getent", "hosts", hostname).Output()
	if err != nil {
		return serverURL
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return serverURL
	}
	return strings.Replace(serverURL, hostname, fields[0], 1)
}
