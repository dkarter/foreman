package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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

var errSatelliteUnauthorized = errors.New("satellite credential was rejected")

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
	credentialUpdates := make(chan struct{}, 1)
	discovery := &discoveryManager{hosts: make(map[string]discoveredHost)}
	if !discoveryDisabled() {
		var err error
		discovery, err = newDiscoveryManager(ctx)
		if err != nil {
			logDiscoveryError(err)
			discovery = &discoveryManager{hosts: make(map[string]discoveredHost)}
		}
	}
	go serveSatelliteController(ctx, serverURL, discovery, credentialUpdates)
	for ctx.Err() == nil {
		credential, err := loadSatelliteCredential()
		if err != nil {
			if !sleep(ctx, 2*time.Second) {
				return ctx.Err()
			}
			continue
		}
		select {
		case <-credentialUpdates:
		default:
		}
		selectedURL := serverURL
		if credential.Endpoint != "" {
			selectedURL = "ws://" + credential.Endpoint + "/satellite"
		}
		authenticatedURL, err := authenticatedSatelliteURL(selectedURL, credential)
		if err != nil {
			return err
		}
		if err := runSatelliteSession(ctx, authenticatedURL, collector, credentialUpdates); err != nil && ctx.Err() == nil {
			if errors.Is(err, errSatelliteUnauthorized) {
				_ = os.Remove(satelliteCredentialPath())
			}
			log.Printf("Foreman satellite disconnected: %v", err)
			if !sleep(ctx, 2*time.Second) {
				return ctx.Err()
			}
		}
	}
	return ctx.Err()
}

func serveSatelliteController(
	ctx context.Context,
	serverURL string,
	discovery *discoveryManager,
	credentialUpdates chan struct{},
) {
	listener, err := net.Listen("tcp", "127.0.0.1:4041")
	if err != nil {
		log.Printf("Foreman credential receiver unavailable: %v", err)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/discovery", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		hostname, _ := os.Hostname()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hosts": discovery.listReachable(r.Context()), "kioskName": hostname,
		})
	})
	mux.HandleFunc("/credential", func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if !discovery.allowsOrigin(origin) && origin != satelliteDashboardOrigin(serverURL) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
		w.Header().Set("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", http.MethodPost+", "+http.MethodDelete)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodDelete {
			if err := os.Remove(satelliteCredentialPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
				http.Error(w, "could not remove credential", http.StatusInternalServerError)
				return
			}
			queueLatest(credentialUpdates, struct{}{})
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var credential pairedDevice
		if json.NewDecoder(r.Body).Decode(&credential) != nil || !validCredential(credential) {
			http.Error(w, "invalid credential", http.StatusBadRequest)
			return
		}
		if err := saveSatelliteCredential(credential); err != nil {
			http.Error(w, "could not save credential", http.StatusInternalServerError)
			return
		}
		queueLatest(credentialUpdates, struct{}{})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/close", func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if !discovery.allowsOrigin(origin) && origin != satelliteDashboardOrigin(serverURL) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
		w.Header().Set("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", http.MethodPost)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		go func() {
			time.Sleep(150 * time.Millisecond)
			_ = exec.Command("pkill", "-f", `[c]hromium.*--kiosk.*(127\.0\.0\.1:4041|air\.local:4040)`).Run()
		}()
	})
	mux.HandleFunc("/", serveDiscoveryPage)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("Foreman credential receiver stopped: %v", err)
	}
}

func serveDiscoveryPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html>
<html><head><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Choose Foreman</title><style>
html{color-scheme:dark;font-family:system-ui;background:#090c0e;color:#f1f3ed}body{margin:0;padding:32px}
main{max-width:720px;margin:auto}h1{font-size:38px;margin-bottom:8px}p{color:#8f989a}
#hosts{display:grid;gap:12px;margin-top:28px}a{padding:20px;border:1px solid #3d4547;background:#111516;color:#f1f3ed;text-decoration:none;font-size:20px}
a small{display:block;margin-top:6px;color:#48d597;font:12px ui-monospace,monospace}a:active{border-color:#48d597}
</style></head><body><main><h1>Choose a Foreman Mac</h1><p>Discovered computers appear automatically. Pair once, then switch between them here.</p><div id="hosts"><p>Searching the local network...</p></div></main>
<script>async function refresh(){try{const {hosts,kioskName}=await(await fetch('/api/discovery')).json();const list=document.querySelector('#hosts');list.replaceChildren();if(!hosts.length){const p=document.createElement('p');p.textContent='No Foreman computers found yet.';list.append(p);return}for(const h of hosts){const address=h.hostname||h.address;const a=document.createElement('a');a.href=` + "`" + `http://${address}:${h.port}/?kiosk=${encodeURIComponent(kioskName)}` + "`" + `;a.textContent=h.name;const small=document.createElement('small');small.textContent=address;a.append(small);list.append(a)}}catch{}}refresh();setInterval(refresh,2000)</script></body></html>`))
}

func loadSatelliteCredential() (pairedDevice, error) {
	data, err := os.ReadFile(satelliteCredentialPath())
	if err != nil {
		return pairedDevice{}, err
	}
	var credential pairedDevice
	if json.Unmarshal(data, &credential) != nil || !validCredential(credential) {
		return pairedDevice{}, errors.New("invalid saved satellite credential")
	}
	return credential, nil
}

func saveSatelliteCredential(credential pairedDevice) error {
	path := satelliteCredentialPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(credential, "", "  ")
	if err != nil {
		return err
	}
	return savePrivateFile(path, append(data, '\n'))
}

func validCredential(credential pairedDevice) bool {
	secret, err := rawBase64.DecodeString(credential.Secret)
	return err == nil && credential.ID != "" && len(secret) == 32
}

func authenticatedSatelliteURL(serverURL string, credential pairedDevice) (string, error) {
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return "", err
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce, err := randomValue(16)
	if err != nil {
		return "", err
	}
	secret, _ := rawBase64.DecodeString(credential.Secret)
	signature := authSignature(secret, http.MethodGet, "/satellite", credential.ID, timestamp, nonce)
	query := parsed.Query()
	query.Set("device", credential.ID)
	query.Set("timestamp", timestamp)
	query.Set("nonce", nonce)
	query.Set("signature", rawBase64.EncodeToString(signature))
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func satelliteDashboardOrigin(serverURL string) string {
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return ""
	}
	if parsed.Scheme == "wss" {
		parsed.Scheme = "https"
	} else {
		parsed.Scheme = "http"
	}
	parsed.Path = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimSuffix(parsed.String(), "/")
}

func satelliteCredentialPath() string {
	if configured := os.Getenv("FOREMAN_DEVICE_CREDENTIAL_PATH"); configured != "" {
		return configured
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "foreman", "device-credential.json")
}

func runSatelliteSession(
	ctx context.Context,
	serverURL string,
	collector *linuxMetricsCollector,
	credentialUpdates <-chan struct{},
) error {
	conn, response, err := websocket.Dial(ctx, serverURL, nil)
	if err != nil {
		if response != nil {
			if response.StatusCode == http.StatusUnauthorized {
				return errSatelliteUnauthorized
			}
			return fmt.Errorf("satellite connection failed: %s", response.Status)
		}
		return err
	}
	defer conn.CloseNow()

	intervals := make(chan int, 1)
	sessionErrors := make(chan error, 1)
	go func() {
		for {
			var message struct {
				Type                string `json:"type"`
				PollIntervalSeconds int    `json:"pollIntervalSeconds"`
			}
			if err := wsjson.Read(ctx, conn, &message); err != nil {
				sessionErrors <- err
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
		case err := <-sessionErrors:
			timer.Stop()
			return err
		case <-credentialUpdates:
			timer.Stop()
			return errors.New("satellite credential changed")
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
