package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

//go:embed web
var webAssets embed.FS

type agent struct {
	PaneID      string `json:"paneId"`
	WorkspaceID string `json:"workspaceId"`
	Workspace   string `json:"workspace"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	Title       string `json:"title"`
	CWD         string `json:"cwd"`
	Focused     bool   `json:"focused"`
}

type dashboard struct {
	Connected bool            `json:"connected"`
	Agents    []agent         `json:"agents"`
	Metrics   resourceMetrics `json:"metrics"`
}

type snapshot struct {
	FocusedPaneID string          `json:"focused_pane_id"`
	Agents        []snapshotAgent `json:"agents"`
	Workspaces    []workspace     `json:"workspaces"`
}

type snapshotAgent struct {
	Agent         string `json:"agent"`
	AgentStatus   string `json:"agent_status"`
	CWD           string `json:"cwd"`
	Focused       bool   `json:"focused"`
	PaneID        string `json:"pane_id"`
	TerminalTitle string `json:"terminal_title_stripped"`
	WorkspaceID   string `json:"workspace_id"`
}

type workspace struct {
	ID    string `json:"workspace_id"`
	Label string `json:"label"`
}

type apiResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *apiError       `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type snapshotResult struct {
	Snapshot snapshot `json:"snapshot"`
}

type subscription struct {
	Type        string `json:"type"`
	PaneID      string `json:"pane_id,omitempty"`
	AgentStatus string `json:"agent_status,omitempty"`
}

type herdrClient struct {
	socket string
}

func (h herdrClient) request(ctx context.Context, method string, params any, result any) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", h.socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	stopContextClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopContextClose()

	request := map[string]any{
		"id":     fmt.Sprintf("foreman-%d", time.Now().UnixNano()),
		"method": method,
		"params": params,
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return err
	}

	var response apiResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return err
	}
	if response.Error != nil {
		return fmt.Errorf("herdr %s: %s (%s)", method, response.Error.Message, response.Error.Code)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(response.Result, result)
}

func (h herdrClient) snapshot(ctx context.Context) (dashboard, error) {
	var result snapshotResult
	if err := h.request(ctx, "session.snapshot", map[string]any{}, &result); err != nil {
		return dashboard{}, err
	}

	workspaceLabels := make(map[string]string, len(result.Snapshot.Workspaces))
	for _, item := range result.Snapshot.Workspaces {
		workspaceLabels[item.ID] = item.Label
	}

	agents := make([]agent, 0, len(result.Snapshot.Agents))
	for _, item := range result.Snapshot.Agents {
		title := item.TerminalTitle
		if title == "" {
			title = filepath.Base(item.CWD)
		}
		agents = append(agents, agent{
			PaneID:      item.PaneID,
			WorkspaceID: item.WorkspaceID,
			Workspace:   workspaceLabels[item.WorkspaceID],
			Kind:        item.Agent,
			Status:      item.AgentStatus,
			Title:       title,
			CWD:         item.CWD,
			Focused:     item.PaneID == result.Snapshot.FocusedPaneID || item.Focused,
		})
	}
	sort.Slice(agents, func(i, j int) bool {
		if agents[i].Status == agents[j].Status {
			return agents[i].Workspace < agents[j].Workspace
		}
		return statusRank(agents[i].Status) < statusRank(agents[j].Status)
	})

	return dashboard{Connected: true, Agents: agents}, nil
}

func statusRank(status string) int {
	switch status {
	case "blocked":
		return 0
	case "working":
		return 1
	case "done":
		return 2
	case "idle":
		return 3
	default:
		return 4
	}
}

func (h herdrClient) subscribe(ctx context.Context, agents []agent) (<-chan struct{}, func(), error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", h.socket)
	if err != nil {
		return nil, nil, err
	}
	stopContextClose := context.AfterFunc(ctx, func() { _ = conn.Close() })

	subscriptions := []subscription{
		{Type: "workspace.created"},
		{Type: "workspace.updated"},
		{Type: "workspace.renamed"},
		{Type: "workspace.closed"},
		{Type: "workspace.focused"},
		{Type: "tab.created"},
		{Type: "tab.closed"},
		{Type: "tab.focused"},
		{Type: "tab.renamed"},
		{Type: "pane.created"},
		{Type: "pane.closed"},
		{Type: "pane.updated"},
		{Type: "pane.focused"},
		{Type: "pane.agent_detected"},
		{Type: "pane.exited"},
	}
	for _, item := range agents {
		subscriptions = append(subscriptions, subscription{
			Type:   "pane.agent_status_changed",
			PaneID: item.PaneID,
		})
	}

	request := map[string]any{
		"id":     fmt.Sprintf("foreman-sub-%d", time.Now().UnixNano()),
		"method": "events.subscribe",
		"params": map[string]any{"subscriptions": subscriptions},
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		stopContextClose()
		conn.Close()
		return nil, nil, err
	}

	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		stopContextClose()
		conn.Close()
		return nil, nil, err
	}
	if !bytes.Contains(line, []byte(`"subscription_started"`)) {
		stopContextClose()
		conn.Close()
		return nil, nil, fmt.Errorf("unexpected Herdr subscription response: %s", strings.TrimSpace(string(line)))
	}

	events := make(chan struct{}, 1)
	streamCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer close(events)
		defer stopContextClose()
		defer conn.Close()
		for {
			if _, err := reader.ReadBytes('\n'); err != nil {
				return
			}
			select {
			case events <- struct{}{}:
			default:
			}
			select {
			case <-streamCtx.Done():
				return
			default:
			}
		}
	}()

	return events, func() {
		cancel()
		conn.Close()
	}, nil
}

type app struct {
	herdr herdrClient

	mu      sync.RWMutex
	state   dashboard
	encoded []byte
	clients map[chan []byte]struct{}
}

func newApp(client herdrClient) *app {
	initial := dashboard{Connected: false, Agents: []agent{}}
	encoded, _ := json.Marshal(map[string]any{"type": "state", "state": initial})
	return &app{
		herdr:   client,
		state:   initial,
		encoded: encoded,
		clients: make(map[chan []byte]struct{}),
	}
}

func (a *app) updateState(update func(*dashboard)) {
	a.mu.Lock()
	next := dashboard{
		Connected: a.state.Connected,
		Agents:    append([]agent(nil), a.state.Agents...),
		Metrics:   a.state.Metrics,
	}
	update(&next)
	encoded, err := json.Marshal(map[string]any{"type": "state", "state": next})
	if err != nil {
		a.mu.Unlock()
		return
	}
	if bytes.Equal(a.encoded, encoded) {
		a.mu.Unlock()
		return
	}
	a.state = next
	a.encoded = encoded
	for client := range a.clients {
		select {
		case client <- encoded:
		default:
			select {
			case <-client:
			default:
			}
			select {
			case client <- encoded:
			default:
			}
		}
	}
	a.mu.Unlock()
}

func (a *app) setDisconnected() {
	a.updateState(func(state *dashboard) {
		state.Connected = false
	})
}

func (a *app) updateHerdr(next dashboard) {
	a.updateState(func(state *dashboard) {
		state.Connected = next.Connected
		state.Agents = next.Agents
	})
}

func (a *app) updateMetrics(metrics resourceMetrics) {
	a.updateState(func(state *dashboard) {
		state.Metrics = metrics
	})
}

func (a *app) monitor(ctx context.Context) {
	for ctx.Err() == nil {
		state, err := a.herdr.snapshot(ctx)
		if err != nil {
			a.setDisconnected()
			log.Printf("Herdr unavailable: %v", err)
			if !sleep(ctx, time.Second) {
				return
			}
			continue
		}
		a.updateHerdr(state)

		events, closeSubscription, err := a.herdr.subscribe(ctx, state.Agents)
		if err != nil {
			a.setDisconnected()
			log.Printf("Herdr subscription failed: %v", err)
			if !sleep(ctx, time.Second) {
				return
			}
			continue
		}

		previousPanes := paneSignature(state.Agents)
		ticker := time.NewTicker(10 * time.Second)
		reconnect := false
		for !reconnect {
			select {
			case <-ctx.Done():
				ticker.Stop()
				closeSubscription()
				return
			case _, ok := <-events:
				if !ok {
					reconnect = true
					continue
				}
			case <-ticker.C:
			}

			refreshCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			refreshed, refreshErr := a.herdr.snapshot(refreshCtx)
			cancel()
			if refreshErr != nil {
				a.setDisconnected()
				reconnect = true
				continue
			}
			a.updateHerdr(refreshed)
			if paneSignature(refreshed.Agents) != previousPanes {
				reconnect = true
			}
		}
		ticker.Stop()
		closeSubscription()
	}
}

func paneSignature(agents []agent) string {
	panes := make([]string, len(agents))
	for i, item := range agents {
		panes[i] = item.PaneID
	}
	sort.Strings(panes)
	return strings.Join(panes, ",")
}

func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (a *app) serveWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		log.Printf("WebSocket accept failed: %v", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1024)

	updates := make(chan []byte, 1)
	a.mu.Lock()
	a.clients[updates] = struct{}{}
	updates <- append([]byte(nil), a.encoded...)
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.clients, updates)
		a.mu.Unlock()
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case message := <-updates:
				writeCtx, writeCancel := context.WithTimeout(ctx, 3*time.Second)
				err := conn.Write(writeCtx, websocket.MessageText, message)
				writeCancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	for {
		var message struct {
			Type   string `json:"type"`
			PaneID string `json:"paneId"`
		}
		if err := wsjson.Read(ctx, conn, &message); err != nil {
			return
		}
		if message.Type != "focus" || !a.hasPane(message.PaneID) {
			continue
		}

		focusCtx, focusCancel := context.WithTimeout(ctx, 2*time.Second)
		err := a.herdr.request(focusCtx, "agent.focus", map[string]any{"target": message.PaneID}, nil)
		focusCancel()
		result := map[string]any{"type": "focusResult", "paneId": message.PaneID, "ok": err == nil}
		if err != nil {
			result["error"] = err.Error()
		}
		encoded, _ := json.Marshal(result)
		select {
		case updates <- encoded:
		default:
		}
	}
}

func (a *app) hasPane(paneID string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, item := range a.state.Agents {
		if item.PaneID == paneID {
			return true
		}
	}
	return false
}

func socketPath() string {
	if configured := os.Getenv("HERDR_SOCKET_PATH"); configured != "" {
		return configured
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "herdr", "herdr.sock")
}

func main() {
	addr := os.Getenv("FOREMAN_ADDR")
	if addr == "" {
		addr = ":4040"
	}
	if socketPath() == "" {
		log.Fatal("could not determine Herdr socket path")
	}

	app := newApp(herdrClient{socket: socketPath()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go app.monitor(ctx)
	go app.monitorMetrics(ctx)

	assets, err := fs.Sub(webAssets, "web")
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/ws", app.serveWebSocket)
	mux.Handle("/", http.FileServerFS(assets))

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("Foreman listening on %s (Herdr socket %s)", addr, socketPath())
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
