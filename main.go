package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
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
	PaneID         string `json:"paneId"`
	WorkspaceID    string `json:"workspaceId"`
	Workspace      string `json:"workspace"`
	Kind           string `json:"kind"`
	Status         string `json:"status"`
	Title          string `json:"title"`
	CWD            string `json:"cwd"`
	Focused        bool   `json:"focused"`
	StateChangeSeq uint64 `json:"-"`
}

type dashboard struct {
	Connected bool            `json:"connected"`
	Agents    []agent         `json:"agents"`
	Metrics   resourceMetrics `json:"metrics"`
	Settings  appSettings     `json:"settings"`
}

type snapshot struct {
	FocusedPaneID string          `json:"focused_pane_id"`
	Agents        []snapshotAgent `json:"agents"`
	Workspaces    []workspace     `json:"workspaces"`
}

type snapshotAgent struct {
	Agent          string `json:"agent"`
	AgentStatus    string `json:"agent_status"`
	CWD            string `json:"cwd"`
	Focused        bool   `json:"focused"`
	PaneID         string `json:"pane_id"`
	TerminalTitle  string `json:"terminal_title_stripped"`
	WorkspaceID    string `json:"workspace_id"`
	StateChangeSeq uint64 `json:"state_change_seq"`
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
			PaneID:         item.PaneID,
			WorkspaceID:    item.WorkspaceID,
			Workspace:      workspaceLabels[item.WorkspaceID],
			Kind:           item.Agent,
			Status:         item.AgentStatus,
			Title:          title,
			CWD:            item.CWD,
			Focused:        item.PaneID == result.Snapshot.FocusedPaneID || item.Focused,
			StateChangeSeq: item.StateChangeSeq,
		})
	}
	sortAgents(agents)

	return dashboard{Connected: true, Agents: agents}, nil
}

func sortAgents(agents []agent) {
	sort.SliceStable(agents, func(i, j int) bool {
		if (agents[i].Status == "blocked") != (agents[j].Status == "blocked") {
			return agents[i].Status == "blocked"
		}
		if agents[i].StateChangeSeq != agents[j].StateChangeSeq {
			return agents[i].StateChangeSeq > agents[j].StateChangeSeq
		}
		if agents[i].Status != agents[j].Status {
			return statusRank(agents[i].Status) < statusRank(agents[j].Status)
		}
		return agents[i].Workspace < agents[j].Workspace
	})
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
	herdr   herdrClient
	pairing *pairingManager

	mu                  sync.RWMutex
	settingsMu          sync.Mutex
	state               dashboard
	encoded             []byte
	clients             map[chan []byte]pairedConnection
	satellites          map[chan []byte]pairedConnection
	settingsFile        string
	settingsChanged     chan struct{}
	satelliteGeneration uint64
}

type pairedConnection struct {
	deviceID string
	cancel   context.CancelFunc
}

func newApp(client herdrClient) *app {
	initial := dashboard{Connected: false, Agents: []agent{}, Settings: defaultSettings()}
	encoded, _ := json.Marshal(map[string]any{"type": "state", "state": initial})
	pairing, _ := newPairingManager("")
	return &app{
		herdr:           client,
		pairing:         pairing,
		state:           initial,
		encoded:         encoded,
		clients:         make(map[chan []byte]pairedConnection),
		satellites:      make(map[chan []byte]pairedConnection),
		settingsChanged: make(chan struct{}, 1),
	}
}

func (a *app) configureSettings(path string) {
	a.settingsFile = path
	a.updateState(func(state *dashboard) {
		state.Settings = loadSettings(path)
	})
}

func (a *app) updateState(update func(*dashboard)) {
	a.mu.Lock()
	next := dashboard{
		Connected: a.state.Connected,
		Agents:    append(make([]agent, 0, len(a.state.Agents)), a.state.Agents...),
		Metrics:   a.state.Metrics,
		Settings:  a.state.Settings,
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
		queueLatest(client, encoded)
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

func (a *app) updateHostMetrics(metrics systemMetrics) {
	a.updateResourceMetrics(func(resource *resourceMetrics) {
		resource.Host = metrics
	})
}

func (a *app) updateForemanMetrics(metrics systemMetrics, connected bool) {
	a.updateResourceMetrics(func(resource *resourceMetrics) {
		resource.Foreman = metrics
		resource.ForemanConnected = connected
	})
}

func (a *app) updateResourceMetrics(update func(*resourceMetrics)) {
	a.mu.Lock()
	metrics := a.state.Metrics
	update(&metrics)
	a.state.Metrics = metrics
	encoded, err := json.Marshal(map[string]any{"type": "metrics", "metrics": metrics})
	if err == nil {
		for client := range a.clients {
			select {
			case client <- encoded:
			default:
			}
		}
	}
	a.mu.Unlock()
}

func (a *app) pollInterval() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.state.Settings.PollIntervalSeconds
}

func (a *app) applySettings(update settingsUpdate) error {
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	a.mu.RLock()
	next := a.state.Settings
	a.mu.RUnlock()
	previous := next
	if update.PollIntervalSeconds != nil {
		if !validPollInterval(*update.PollIntervalSeconds) {
			return fmt.Errorf("poll interval must be 5, 10, 30, or 60 seconds")
		}
		next.PollIntervalSeconds = *update.PollIntervalSeconds
	}
	if update.CompactMode != nil {
		next.CompactMode = *update.CompactMode
	}
	if next == previous {
		return nil
	}
	if a.settingsFile != "" {
		if err := saveSettings(a.settingsFile, next); err != nil {
			return err
		}
	}
	a.updateState(func(state *dashboard) {
		state.Settings = next
	})
	if next.PollIntervalSeconds != previous.PollIntervalSeconds {
		queueLatest(a.settingsChanged, struct{}{})
		a.broadcastSatelliteSettings(next.PollIntervalSeconds)
	}
	return nil
}

func queueLatest[T any](channel chan T, value T) {
	select {
	case channel <- value:
		return
	default:
	}
	select {
	case <-channel:
	default:
	}
	select {
	case channel <- value:
	default:
	}
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
	if !requestIsLoopback(r) && r.TLS == nil {
		http.Error(w, "TLS required", http.StatusUpgradeRequired)
		return
	}
	if !requestIsLoopback(r) && !a.pairing.authorize(r, "/ws") {
		http.Error(w, "kiosk pairing required", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		log.Printf("WebSocket accept failed: %v", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1024)

	updates := make(chan []byte, 1)
	results := make(chan []byte, 4)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	a.mu.Lock()
	a.clients[updates] = pairedConnection{deviceID: r.URL.Query().Get("device"), cancel: cancel}
	initialState, _ := json.Marshal(map[string]any{"type": "state", "state": a.state})
	updates <- initialState
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.clients, updates)
		a.mu.Unlock()
	}()

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
			case message := <-results:
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
			Type     string         `json:"type"`
			PaneID   string         `json:"paneId"`
			Settings settingsUpdate `json:"settings"`
		}
		if err := wsjson.Read(ctx, conn, &message); err != nil {
			return
		}
		if message.Type == "settings" {
			if err := a.applySettings(message.Settings); err != nil {
				encoded, _ := json.Marshal(map[string]any{"type": "settingsResult", "ok": false, "error": err.Error()})
				queueLatest(results, encoded)
			}
			continue
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
		queueLatest(results, encoded)
	}
}

func (a *app) serveSettings(w http.ResponseWriter, r *http.Request) {
	if !requestIsLoopback(r) {
		http.Error(w, "settings API is local-only", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		a.mu.RLock()
		settings := a.state.Settings
		a.mu.RUnlock()
		_ = json.NewEncoder(w).Encode(settings)
		return
	}
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var update settingsUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		http.Error(w, "invalid settings", http.StatusBadRequest)
		return
	}
	if err := a.applySettings(update); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func requestIsLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	return err == nil && net.ParseIP(host).IsLoopback()
}

func (a *app) broadcastSatelliteSettings(interval int) {
	message, _ := json.Marshal(map[string]any{"type": "settings", "pollIntervalSeconds": interval})
	a.mu.RLock()
	defer a.mu.RUnlock()
	for satellite := range a.satellites {
		queueLatest(satellite, message)
	}
}

func (a *app) serveSatellite(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil {
		http.Error(w, "TLS required", http.StatusUpgradeRequired)
		return
	}
	if !a.pairing.authorize(r, "/satellite") {
		http.Error(w, "kiosk pairing required", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	updates := make(chan []byte, 1)
	a.mu.Lock()
	a.satelliteGeneration++
	generation := a.satelliteGeneration
	a.satellites[updates] = pairedConnection{deviceID: r.URL.Query().Get("device"), cancel: cancel}
	interval := a.state.Settings.PollIntervalSeconds
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.satellites, updates)
		isCurrent := generation == a.satelliteGeneration
		a.mu.Unlock()
		if isCurrent {
			a.updateForemanMetrics(systemMetrics{}, false)
		}
	}()

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
	initial, _ := json.Marshal(map[string]any{"type": "settings", "pollIntervalSeconds": interval})
	queueLatest(updates, initial)

	for {
		var message struct {
			Type    string        `json:"type"`
			Metrics systemMetrics `json:"metrics"`
		}
		if err := wsjson.Read(ctx, conn, &message); err != nil {
			return
		}
		if message.Type == "metrics" {
			a.mu.RLock()
			isCurrent := generation == a.satelliteGeneration
			a.mu.RUnlock()
			if isCurrent {
				a.updateForemanMetrics(message.Metrics, true)
			}
		}
	}
}

func (a *app) servePairing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/pairing/request":
		var request struct {
			Name      string `json:"name"`
			PublicKey string `json:"publicKey"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			http.Error(w, "invalid pairing request", http.StatusBadRequest)
			return
		}
		pending, serverKey, err := a.pairing.begin(request.Name, request.PublicKey, r.RemoteAddr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": pending.ID, "code": pending.Code, "serverPublicKey": serverKey,
			"expiresAt": pending.ExpiresAt,
		})
	case r.Method == http.MethodPost && r.URL.Path == "/api/pairing/confirm":
		var request struct {
			ID string `json:"id"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || a.pairing.confirm(request.ID) != nil {
			http.Error(w, "pairing request is not available", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && r.URL.Path == "/api/pairing/complete":
		var request struct {
			ID string `json:"id"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			http.Error(w, "invalid pairing completion", http.StatusBadRequest)
			return
		}
		a.pairing.complete(request.ID)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/api/pairing/status":
		status, credential, err := a.pairing.status(r.URL.Query().Get("id"))
		if err != nil {
			http.Error(w, "could not complete pairing", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "credential": credential})
	case r.Method == http.MethodGet && r.URL.Path == "/api/pairing/device":
		_ = json.NewEncoder(w).Encode(map[string]any{"paired": a.pairing.hasDevice(r.URL.Query().Get("id"))})
	case r.URL.Path == "/api/pairing/control":
		if !requestIsLoopback(r) {
			http.Error(w, "pairing control is local-only", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(a.pairing.pendingState())
			return
		}
		if r.Method == http.MethodDelete {
			if err := a.pairing.unpair(r.URL.Query().Get("device")); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			a.disconnectPairedClients(r.URL.Query().Get("device"))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost {
			var request struct {
				ID      string `json:"id"`
				Approve bool   `json:"approve"`
				Action  string `json:"action"`
			}
			if json.NewDecoder(r.Body).Decode(&request) != nil {
				http.Error(w, "invalid pairing control request", http.StatusBadRequest)
				return
			}
			if request.Action == "enable" {
				_ = json.NewEncoder(w).Encode(map[string]any{"enabledUntil": a.pairing.enable()})
				return
			}
			if a.pairing.decide(request.ID, request.Approve) != nil {
				http.Error(w, "pairing request is not available", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	default:
		http.NotFound(w, r)
	}
}

func (a *app) disconnectPairedClients(deviceID string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, connection := range a.clients {
		if deviceID == "" || connection.deviceID == deviceID {
			connection.cancel()
		}
	}
	for _, connection := range a.satellites {
		if deviceID == "" || connection.deviceID == deviceID {
			connection.cancel()
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

func embeddedDashboardHandler() (http.Handler, error) {
	assets, err := fs.Sub(webAssets, "web")
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServerFS(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	}), nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "satellite" {
		log.Printf("Foreman satellite starting")
		if err := runSatellite(context.Background()); err != nil {
			log.Fatal(err)
		}
		return
	}
	addr := os.Getenv("FOREMAN_ADDR")
	if addr == "" {
		addr = ":4040"
	}
	if socketPath() == "" {
		log.Fatal("could not determine Herdr socket path")
	}

	app := newApp(herdrClient{socket: socketPath()})
	pairing, err := newPairingManager(pairingPath())
	if err != nil {
		log.Fatalf("Could not load pairing state: %v", err)
	}
	app.pairing = pairing
	hasTLSDevices := false
	for _, device := range pairing.devices {
		hasTLSDevices = hasTLSDevices || device.TransportVersion >= 2
	}
	identity, err := loadTLSIdentity(tlsIdentityPath(), pairing.hostID, pairing.hostName, hasTLSDevices)
	if err != nil {
		log.Fatalf("Could not load TLS identity: %v", err)
	}
	if err := pairing.removeLegacyDevices(); err != nil {
		log.Fatalf("Could not migrate pairing state to TLS: %v", err)
	}
	if len(pairing.devices) > 0 && !hasTLSDevices {
		log.Printf("Legacy kiosk pairings cleared; kiosks must pair once to trust the TLS identity")
	}
	app.pairing.tlsFingerprint = identity.Fingerprint
	app.pairing.tlsPort = defaultTLSPort
	app.configureSettings(settingsPath())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !discoveryDisabled() {
		shutdownDiscovery, discoveryErr := advertiseForeman(app.pairing, foremanPort(addr))
		logDiscoveryError(discoveryErr)
		if shutdownDiscovery != nil {
			defer shutdownDiscovery()
		}
	}
	go app.monitor(ctx)
	go app.monitorMetrics(ctx)

	dashboardHandler, err := embeddedDashboardHandler()
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/ws", app.serveWebSocket)
	mux.HandleFunc("/satellite", app.serveSatellite)
	mux.HandleFunc("/api/settings", app.serveSettings)
	mux.HandleFunc("/api/pairing/", app.servePairing)
	mux.Handle("/", dashboardHandler)

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	tlsServer := &http.Server{
		Addr: ":4042", Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{identity.Certificate}, MinVersion: tls.VersionTLS13,
		},
	}
	serverErrors := make(chan error, 2)
	go func() {
		log.Printf("Foreman TLS listening on %s", tlsServer.Addr)
		serverErrors <- tlsServer.ListenAndServeTLS("", "")
	}()
	log.Printf("Foreman listening on %s (Herdr socket %s)", addr, socketPath())
	go func() {
		serverErrors <- server.ListenAndServe()
	}()
	serverErr := <-serverErrors
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
	_ = tlsServer.Shutdown(shutdownCtx)
	if !errors.Is(serverErr, http.ErrServerClosed) {
		log.Printf("Foreman server failed: %v", serverErr)
	}
}
