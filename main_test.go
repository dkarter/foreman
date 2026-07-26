package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"
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

func TestAggregateAgentStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		agents []agent
		want   string
	}{
		{name: "no agents", want: "idle"},
		{name: "all idle", agents: []agent{{Status: "idle"}, {Status: "idle"}}, want: "idle"},
		{name: "done", agents: []agent{{Status: "idle"}, {Status: "done"}}, want: "done"},
		{name: "working", agents: []agent{{Status: "idle"}, {Status: "working"}}, want: "working"},
		{name: "working wins over done", agents: []agent{{Status: "done"}, {Status: "working"}}, want: "working"},
		{name: "blocked wins", agents: []agent{{Status: "working"}, {Status: "blocked"}}, want: "blocked"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := aggregateAgentStatus(test.agents); got != test.want {
				t.Fatalf("aggregateAgentStatus() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestStatusOmitsStaleAgentsWhenDisconnected(t *testing.T) {
	t.Parallel()

	app := newApp(herdrClient{})
	app.updateHerdr(dashboard{Connected: true, Agents: []agent{{Status: "blocked"}}})
	app.setDisconnected()
	request := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	app.serveStatus(response, request)

	var body map[string]any
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil {
		t.Fatalf("unexpected status response: %d %s", response.Code, response.Body.String())
	}
	if _, exists := body["agentStatus"]; exists {
		t.Fatalf("disconnected status exposed stale agents: %#v", body)
	}
}

func TestAgentSortPrioritizesAttentionThenRecency(t *testing.T) {
	t.Parallel()

	agents := []agent{
		{PaneID: "old", Status: "idle", StateChangeSeq: 2},
		{PaneID: "new", Status: "working", StateChangeSeq: 8},
		{PaneID: "blocked", Status: "blocked", StateChangeSeq: 1},
	}
	sortAgents(agents)
	if agents[0].PaneID != "blocked" || agents[1].PaneID != "new" || agents[2].PaneID != "old" {
		t.Fatalf("unexpected agent order: %#v", agents)
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
	if _, ok := parseMemoryPressure("unexpected output"); ok {
		t.Fatal("invalid memory pressure should not produce a metric")
	}
	idle, total, ok := parseProcStat("cpu  10 2 3 20 5 0 0 0\n")
	if !ok || idle != 25 || total != 40 {
		t.Fatalf("parseProcStat() = %v, %v, want 25, 40", idle, total)
	}
	used, total, ok := parseProcMeminfo("MemTotal: 1000 kB\nMemAvailable: 400 kB\n")
	if !ok || used != 600*1024 || total != 1000*1024 {
		t.Fatalf("parseProcMeminfo() = %v, %v", used, total)
	}
}

func TestStateUpdatesPreserveIndependentData(t *testing.T) {
	t.Parallel()

	app := newApp(herdrClient{})
	host := systemMetrics{CPU: floatValue(12), RAM: floatValue(34)}
	foreman := systemMetrics{CPU: floatValue(0.2), RAM: floatValue(18)}
	app.updateHostMetrics(host)
	app.updateForemanMetrics(foreman, true)
	metrics := resourceMetrics{Host: host, Foreman: foreman, ForemanConnected: true}
	app.updateHerdr(dashboard{Connected: true, Agents: []agent{{PaneID: "w1:p1"}}})

	if !reflect.DeepEqual(app.state.Metrics, metrics) {
		t.Fatalf("Herdr update replaced metrics: %#v", app.state.Metrics)
	}
	if !app.state.Connected || len(app.state.Agents) != 1 {
		t.Fatalf("unexpected Herdr state: %#v", app.state)
	}

	app.updateHostMetrics(systemMetrics{CPU: floatValue(56), RAM: floatValue(78)})
	if !app.state.Connected || len(app.state.Agents) != 1 {
		t.Fatalf("metrics update replaced Herdr state: %#v", app.state)
	}
}

func TestSettingsPersist(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "settings.json")
	app := newApp(herdrClient{})
	app.settingsFile = path
	poll := 30
	compact := true
	if err := app.applySettings(settingsUpdate{PollIntervalSeconds: &poll, CompactMode: &compact}); err != nil {
		t.Fatal(err)
	}
	if got := loadSettings(path); got.PollIntervalSeconds != 30 || !got.CompactMode {
		t.Fatalf("unexpected persisted settings: %#v", got)
	}
}

func TestPairingCeremonyAndAuthenticatedRequest(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "pairing.json")
	manager, err := newPairingManager(path)
	if err != nil {
		t.Fatal(err)
	}
	manager.enable()
	manager.tlsFingerprint, _ = randomValue(32)
	manager.tlsPort = defaultTLSPort
	clientKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pending, encodedServerKey, err := manager.begin(
		"shop kiosk", rawBase64.EncodeToString(clientKey.PublicKey().Bytes()), "192.0.2.10:1234",
	)
	if err != nil {
		t.Fatal(err)
	}
	serverKeyBytes, _ := rawBase64.DecodeString(encodedServerKey)
	serverKey, err := ecdh.P256().NewPublicKey(serverKeyBytes)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := clientKey.ECDH(serverKey)
	if err != nil {
		t.Fatal(err)
	}
	codeMAC := hmac.New(sha256.New, shared)
	codeMAC.Write([]byte("foreman-pairing-code"))
	codeMAC.Write(clientKey.PublicKey().Bytes())
	codeMAC.Write(serverKeyBytes)
	if pending.Code != fmt.Sprintf("%06d", binary.BigEndian.Uint32(codeMAC.Sum(nil)[:4])%1000000) {
		t.Fatal("pairing codes do not match")
	}
	if err := manager.confirm(pending.ID); err != nil {
		t.Fatal(err)
	}
	if err := manager.decide(pending.ID, true); err != nil {
		t.Fatal(err)
	}
	status, encrypted, err := manager.status(pending.ID)
	if err != nil || status != "paired" || encrypted == nil {
		t.Fatalf("pairing status = %q, %#v, %v", status, encrypted, err)
	}
	key := sha256.Sum256(append(append(shared, clientKey.PublicKey().Bytes()...), serverKeyBytes...))
	block, _ := aes.NewCipher(key[:])
	aead, _ := cipher.NewGCM(block)
	nonce, _ := rawBase64.DecodeString(encrypted.Nonce)
	ciphertext, _ := rawBase64.DecodeString(encrypted.Ciphertext)
	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatal(err)
	}
	var credential pairedDevice
	if json.Unmarshal(plaintext, &credential) != nil || credential.Name != "shop kiosk" {
		t.Fatalf("unexpected credential: %#v", credential)
	}
	if credential.TLSCertSHA256 != manager.tlsFingerprint || credential.TransportVersion != 2 {
		t.Fatalf("pairing credential is missing pinned TLS identity: %#v", credential)
	}

	timestamp := fmt.Sprint(time.Now().Unix())
	nonceValue := "one-time-nonce"
	secret, _ := rawBase64.DecodeString(credential.Secret)
	signature := rawBase64.EncodeToString(authSignature(secret, http.MethodGet, "/ws", credential.ID, timestamp, nonceValue))
	request := httptest.NewRequest(http.MethodGet, "/ws?device="+credential.ID+"&timestamp="+timestamp+"&nonce="+nonceValue+"&signature="+signature, nil)
	if !manager.authorize(request, "/ws") {
		t.Fatal("paired request was rejected")
	}
	if manager.authorize(request, "/ws") {
		t.Fatal("replayed request was accepted")
	}

	reloaded, err := newPairingManager(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.devices) != 1 || reloaded.hostID != manager.hostID {
		t.Fatalf("pairing store did not reload: %#v", reloaded)
	}
}

func TestDiscoveryEntryUsesStableHostIdentity(t *testing.T) {
	t.Parallel()
	entry := &zeroconf.ServiceEntry{
		ServiceRecord: zeroconf.ServiceRecord{Instance: "Foreman on studio", Service: foremanService, Domain: "local."},
		HostName:      "studio.local.",
		Port:          4040,
		Text:          []string{"id=host-123", "name=Studio Mac", "protocol=2", "tlsPort=4042"},
		AddrIPv4:      []net.IP{net.ParseIP("192.0.2.20")},
	}
	host, ok := discoveredHostFromEntry(entry)
	if !ok || host.ID != "host-123" || host.dashboardURL() != "http://studio.local:4040" {
		t.Fatalf("unexpected discovered host: %#v", host)
	}
}

func TestAvahiDiscoveryParsesAndRemovesHost(t *testing.T) {
	t.Parallel()
	manager := &discoveryManager{hosts: make(map[string]discoveredHost)}
	manager.applyAvahiLine(`=;wlan0;IPv4;Foreman\032on\032air\.local;_foreman._tcp;local;air.local;10.0.0.80;4040;"protocol=2" "tlsPort=4042" "name=MacBook\032Air" "id=host-123"`)
	hosts := manager.list()
	if len(hosts) != 1 || hosts[0].Name != "MacBook Air" || hosts[0].dashboardURL() != "http://air.local:4040" {
		t.Fatalf("unexpected Avahi host: %#v", hosts)
	}
	manager.applyAvahiLine(`-;wlan0;IPv4;Foreman\032on\032air\.local;_foreman._tcp;local`)
	if len(manager.list()) != 0 {
		t.Fatalf("Avahi removal did not remove host: %#v", manager.hosts)
	}
}

func TestPairingStoreRevokesOneKioskWithoutAffectingOthers(t *testing.T) {
	t.Parallel()
	manager, err := newPairingManager(filepath.Join(t.TempDir(), "pairing.json"))
	if err != nil {
		t.Fatal(err)
	}
	firstSecret, _ := randomValue(32)
	secondSecret, _ := randomValue(32)
	manager.devices["first"] = pairedDevice{ID: "first", Name: "Kitchen", Secret: firstSecret}
	manager.devices["second"] = pairedDevice{ID: "second", Name: "Office", Secret: secondSecret}
	if err := manager.saveLocked(); err != nil {
		t.Fatal(err)
	}
	if err := manager.unpair("first"); err != nil {
		t.Fatal(err)
	}
	if manager.hasDevice("first") || !manager.hasDevice("second") {
		t.Fatalf("unexpected paired devices: %#v", manager.devices)
	}
	reloaded, err := newPairingManager(manager.path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.hasDevice("first") || !reloaded.hasDevice("second") {
		t.Fatalf("revocation was not persisted: %#v", reloaded.devices)
	}
}

func TestLoopbackRequestsAreRecognized(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4040/ws", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	if !requestIsLoopback(request) {
		t.Fatal("loopback request was not recognized")
	}
	request.RemoteAddr = "192.0.2.10:12345"
	if requestIsLoopback(request) {
		t.Fatal("remote request was recognized as loopback")
	}
}

func TestPairingRetryReplacesRequestFromSameKiosk(t *testing.T) {
	t.Parallel()
	manager, err := newPairingManager(filepath.Join(t.TempDir(), "pairing.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager.enable()
	firstKey, _ := ecdh.P256().GenerateKey(rand.Reader)
	first, _, err := manager.begin("Kitchen", rawBase64.EncodeToString(firstKey.PublicKey().Bytes()), "192.0.2.10:1000")
	if err != nil {
		t.Fatal(err)
	}
	secondKey, _ := ecdh.P256().GenerateKey(rand.Reader)
	second, _, err := manager.begin("Kitchen", rawBase64.EncodeToString(secondKey.PublicKey().Bytes()), "192.0.2.10:2000")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || manager.pending[first.ID] != nil || manager.pending[second.ID] == nil {
		t.Fatalf("retry did not replace the original request: %#v", manager.pending)
	}
	otherKey, _ := ecdh.P256().GenerateKey(rand.Reader)
	if _, _, err := manager.begin("Office", rawBase64.EncodeToString(otherKey.PublicKey().Bytes()), "192.0.2.11:1000"); err == nil {
		t.Fatal("a different kiosk replaced the pending request")
	}
}

func TestTLSIdentityPersistsAndPinRejectsDifferentHost(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "tls-identity.json")
	identity, err := loadTLSIdentity(path, "host-1", "studio", false)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadTLSIdentity(path, "host-1", "studio", true)
	if err != nil || reloaded.Fingerprint != identity.Fingerprint {
		t.Fatalf("TLS identity did not persist: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("TLS identity permissions = %v", info.Mode().Perm())
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{identity.Certificate}, MinVersion: tls.VersionTLS13}
	server.StartTLS()
	defer server.Close()
	client, err := pinnedHTTPClient(identity.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	wrongPin, _ := randomValue(32)
	wrongClient, _ := pinnedHTTPClient(wrongPin)
	if _, err := wrongClient.Get(server.URL); err == nil {
		t.Fatal("client accepted a different TLS certificate pin")
	}
}

func TestAuthenticatedSatelliteURLRequiresWSS(t *testing.T) {
	t.Parallel()
	secret, _ := randomValue(32)
	credential := pairedDevice{ID: "device-1", Secret: secret}
	if _, err := authenticatedSatelliteURL("ws://host.test/satellite", credential); err == nil {
		t.Fatal("plaintext satellite URL was accepted")
	}
	result, err := authenticatedSatelliteURL("wss://host.test/satellite", credential)
	if err != nil || !strings.HasPrefix(result, "wss://host.test/satellite?") {
		t.Fatalf("authenticated WSS URL = %q, %v", result, err)
	}
}

func TestRemoveLegacyDevicesPreservesTLSDevices(t *testing.T) {
	t.Parallel()
	manager, err := newPairingManager(filepath.Join(t.TempDir(), "pairing.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager.devices["legacy"] = pairedDevice{ID: "legacy", TransportVersion: 1}
	manager.devices["current"] = pairedDevice{ID: "current", TransportVersion: 2}
	if err := manager.removeLegacyDevices(); err != nil {
		t.Fatal(err)
	}
	if _, exists := manager.devices["legacy"]; exists {
		t.Fatal("legacy device was preserved")
	}
	if _, exists := manager.devices["current"]; !exists {
		t.Fatal("TLS device was removed")
	}
}
