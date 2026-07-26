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
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

func runSatellite(ctx context.Context) error {
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
	go serveSatelliteController(ctx, discovery, credentialUpdates)
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
		if credential.Endpoint == "" {
			_ = os.Remove(satelliteCredentialPath())
			continue
		}
		selectedURL := "wss://" + credential.Endpoint + "/satellite"
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
	discovery *discoveryManager,
	credentialUpdates chan struct{},
) {
	listener, err := net.Listen("tcp", "127.0.0.1:4041")
	if err != nil {
		log.Printf("Foreman credential receiver unavailable: %v", err)
		return
	}
	defer listener.Close()
	mux := http.NewServeMux()
	dashboardHandler, err := embeddedDashboardHandler()
	if err != nil {
		log.Printf("Foreman dashboard unavailable: %v", err)
		return
	}
	mux.HandleFunc("/api/discovery", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		hostname, _ := os.Hostname()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hosts": discovery.listReachable(r.Context()), "kioskName": hostname,
		})
	})
	mux.Handle("/api/pairing/", selectedHostProxy(discovery, false))
	mux.Handle("/ws", selectedHostProxy(discovery, true))
	mux.HandleFunc("/credential", func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if !controllerOrigin(origin) {
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
			credential, loadErr := loadSatelliteCredential()
			if loadErr == nil && credential.HostID != r.URL.Query().Get("host") {
				http.Error(w, "another host is currently selected", http.StatusConflict)
				return
			}
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
		if host, ok := discovery.get(r.URL.Query().Get("host")); !ok || host.ID != credential.HostID {
			http.Error(w, "selected host does not match credential", http.StatusBadRequest)
			return
		} else {
			credential.Endpoint = net.JoinHostPort(host.Address, strconv.Itoa(host.TLSPort))
		}
		if current, err := loadSatelliteCredential(); err == nil && reflect.DeepEqual(current, credential) {
			w.WriteHeader(http.StatusNoContent)
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
		if !controllerOrigin(origin) {
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
	mux.HandleFunc("/choose", serveDiscoveryPage)
	mux.Handle("/assets/", dashboardHandler)
	mux.Handle("/app.js", dashboardHandler)
	mux.Handle("/styles.css", dashboardHandler)
	mux.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		clone := r.Clone(r.Context())
		clone.URL.Path = "/"
		dashboardHandler.ServeHTTP(w, clone)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/choose", http.StatusTemporaryRedirect)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("Foreman credential receiver stopped: %v", err)
	}
}

func selectedHostProxy(discovery *discoveryManager, pinned bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostID := r.URL.Query().Get("host")
		host, ok := discovery.get(hostID)
		if !ok {
			http.Error(w, "selected Foreman host is unavailable", http.StatusServiceUnavailable)
			return
		}
		scheme := "http"
		port := host.Port
		var transport http.RoundTripper
		if pinned || r.URL.Path == "/api/pairing/device" {
			credential, err := loadSatelliteCredential()
			if err != nil || credential.HostID != hostID {
				http.Error(w, "pairing required", http.StatusUnauthorized)
				return
			}
			client, err := pinnedHTTPClient(credential.TLSCertSHA256)
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			scheme = "https"
			port = host.TLSPort
			transport = client.Transport
			defer client.CloseIdleConnections()
		}
		target, _ := url.Parse(scheme + "://" + net.JoinHostPort(host.Address, strconv.Itoa(port)))
		proxy := httputil.NewSingleHostReverseProxy(target)
		if transport != nil {
			proxy.Transport = transport
		}
		originalDirector := proxy.Director
		proxy.Director = func(request *http.Request) {
			originalDirector(request)
			query := request.URL.Query()
			query.Del("host")
			request.URL.RawQuery = query.Encode()
		}
		proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("Foreman host proxy failed: %v", err)
			http.Error(w, "secure connection to Foreman failed", http.StatusBadGateway)
		}
		proxy.ServeHTTP(w, r)
	})
}

func serveDiscoveryPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/choose" {
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
<script>async function refresh(){try{const {hosts,kioskName}=await(await fetch('/api/discovery')).json();const list=document.querySelector('#hosts');list.replaceChildren();if(!hosts.length){const p=document.createElement('p');p.textContent='No Foreman computers found yet.';list.append(p);return}for(const h of hosts){const address=h.hostname||h.address;const a=document.createElement('a');a.href=` + "`" + `/dashboard?host=${encodeURIComponent(h.id)}&kiosk=${encodeURIComponent(kioskName)}` + "`" + `;a.textContent=h.name;const small=document.createElement('small');small.textContent=address;a.append(small);list.append(a)}}catch{}}refresh();setInterval(refresh,2000)</script></body></html>`))
}

func loadSatelliteCredential() (pairedDevice, error) {
	data, err := os.ReadFile(satelliteCredentialPath())
	if err != nil {
		return pairedDevice{}, err
	}
	var credential pairedDevice
	if json.Unmarshal(data, &credential) != nil || !validCredential(credential) || credential.Endpoint == "" {
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
	fingerprint, fingerprintErr := rawBase64.DecodeString(credential.TLSCertSHA256)
	return err == nil && fingerprintErr == nil && credential.TransportVersion == 2 &&
		credential.ID != "" && credential.HostID != "" &&
		len(secret) == 32 && len(fingerprint) == 32
}

func controllerOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Scheme == "http" && parsed.Host == "127.0.0.1:4041"
}

func authenticatedSatelliteURL(serverURL string, credential pairedDevice) (string, error) {
	parsed, err := url.Parse(serverURL)
	if err != nil || parsed.Scheme != "wss" {
		return "", errors.New("satellite transport requires WSS")
	}
	if parsed.Host == "" {
		return "", errors.New("satellite transport requires a host")
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
	credential, credentialErr := loadSatelliteCredential()
	if credentialErr != nil {
		return credentialErr
	}
	client, err := pinnedHTTPClient(credential.TLSCertSHA256)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	conn, response, err := websocket.Dial(ctx, serverURL, &websocket.DialOptions{HTTPClient: client})
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
