package main

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSnapshotBuildsDashboard(t *testing.T) {
	t.Parallel()

	socket := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()

		var request map[string]any
		_ = json.NewDecoder(conn).Decode(&request)
		_ = json.NewEncoder(conn).Encode(map[string]any{
			"id": "test",
			"result": map[string]any{
				"type": "session_snapshot",
				"snapshot": map[string]any{
					"focused_pane_id": "w1:p1",
					"workspaces":      []map[string]any{{"workspace_id": "w1", "label": "foreman"}},
					"agents": []map[string]any{{
						"agent": "opencode", "agent_status": "working", "cwd": "/tmp/foreman",
						"pane_id": "w1:p1", "workspace_id": "w1", "terminal_title_stripped": "Build UI",
					}},
				},
			},
		})
	}()

	dashboard, err := (herdrClient{socket: socket}).snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !dashboard.Connected || len(dashboard.Agents) != 1 {
		t.Fatalf("unexpected dashboard: %#v", dashboard)
	}
	agent := dashboard.Agents[0]
	if agent.Workspace != "foreman" || agent.Title != "Build UI" || !agent.Focused {
		t.Fatalf("unexpected agent: %#v", agent)
	}
}

func TestRequestCancel(t *testing.T) {
	t.Parallel()

	socket := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		<-time.After(time.Second)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = (herdrClient{socket: socket}).request(ctx, "session.snapshot", map[string]any{}, nil)
	if err == nil {
		t.Fatal("request should fail when its context is cancelled")
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("cancelled request took %s", elapsed)
	}
}

func TestStatusRankPrioritizesAttention(t *testing.T) {
	t.Parallel()

	if statusRank("blocked") >= statusRank("working") {
		t.Fatal("blocked agents should sort before working agents")
	}
	if statusRank("working") >= statusRank("idle") {
		t.Fatal("working agents should sort before idle agents")
	}
}

func TestMetricParsing(t *testing.T) {
	t.Parallel()

	if got, ok := parseCPUList(" 1.2\n 3.4\n"); !ok || got != 4.6 {
		t.Fatalf("parseCPUList() = %v, want 4.6", got)
	}
	if got, ok := parseMemoryPressure("System-wide memory free percentage: 62%\n"); !ok || got != 38 {
		t.Fatalf("parseMemoryPressure() = %v, want 38", got)
	}
	cpu, ram, ok := parseProcessMetrics(" 0.3 18432\n")
	if !ok || cpu != 0.3 || ram != 18 {
		t.Fatalf("parseProcessMetrics() = %v, %v, want 0.3, 18", cpu, ram)
	}
	if _, ok := parseMemoryPressure("unexpected output"); ok {
		t.Fatal("invalid memory pressure should not produce a metric")
	}
}

func TestStateUpdatesPreserveIndependentData(t *testing.T) {
	t.Parallel()

	app := newApp(herdrClient{})
	metrics := resourceMetrics{
		HostCPU: metricValue(12), HostRAM: metricValue(34),
		ForemanCPU: metricValue(0.2), ForemanRAMMiB: metricValue(18),
	}
	app.updateMetrics(metrics)
	app.updateHerdr(dashboard{Connected: true, Agents: []agent{{PaneID: "w1:p1"}}})

	if !reflect.DeepEqual(app.state.Metrics, metrics) {
		t.Fatalf("Herdr update replaced metrics: %#v", app.state.Metrics)
	}
	if !app.state.Connected || len(app.state.Agents) != 1 {
		t.Fatalf("unexpected Herdr state: %#v", app.state)
	}

	nextMetrics := resourceMetrics{HostCPU: metricValue(56), HostRAM: metricValue(78)}
	app.updateMetrics(nextMetrics)
	if !app.state.Connected || len(app.state.Agents) != 1 {
		t.Fatalf("metrics update replaced Herdr state: %#v", app.state)
	}
}
