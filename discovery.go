package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

const foremanService = "_foreman._tcp"

type discoveredHost struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Instance string    `json:"-"`
	Hostname string    `json:"hostname"`
	Address  string    `json:"address"`
	Port     int       `json:"port"`
	TLSPort  int       `json:"tlsPort"`
	LastSeen time.Time `json:"-"`
}

type discoveryManager struct {
	mu    sync.RWMutex
	hosts map[string]discoveredHost
}

func advertiseForeman(manager *pairingManager, port int) (func(), error) {
	manager.mu.Lock()
	hostID := manager.hostID
	hostName := manager.hostName
	manager.mu.Unlock()
	instance := "Foreman on " + hostName
	text := []string{
		"id=" + hostID,
		"name=" + hostName,
		"protocol=2",
		"tlsPort=" + strconv.Itoa(defaultTLSPort),
	}
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithCancel(context.Background())
		arguments := []string{"-R", instance, foremanService, "local.", strconv.Itoa(port)}
		arguments = append(arguments, text...)
		command := exec.CommandContext(ctx, "/usr/bin/dns-sd", arguments...)
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if err := command.Start(); err != nil {
			cancel()
			return nil, err
		}
		go func() { _ = command.Wait() }()
		return cancel, nil
	}
	server, err := zeroconf.Register(instance, foremanService, "local.", port, text, nil)
	if err != nil {
		return nil, err
	}
	return server.Shutdown, nil
}

func newDiscoveryManager(ctx context.Context) (*discoveryManager, error) {
	manager := &discoveryManager{hosts: make(map[string]discoveredHost)}
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("avahi-browse"); err == nil {
			go manager.browseAvahi(ctx)
			return manager, nil
		}
	}
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, err
	}
	entries := make(chan *zeroconf.ServiceEntry, 16)
	go manager.collect(ctx, entries)
	if err := resolver.Browse(ctx, foremanService, "local.", entries); err != nil {
		return nil, err
	}
	return manager, nil
}

func (manager *discoveryManager) browseAvahi(ctx context.Context) {
	for ctx.Err() == nil {
		output, err := exec.CommandContext(ctx, "avahi-browse", "-r", "-t", "-k", "-p", foremanService).Output()
		if err != nil && ctx.Err() == nil {
			logDiscoveryError(err)
		}
		snapshot := &discoveryManager{hosts: make(map[string]discoveredHost)}
		for _, line := range strings.Split(string(output), "\n") {
			snapshot.applyAvahiLine(line)
		}
		manager.mu.Lock()
		manager.hosts = snapshot.hosts
		manager.mu.Unlock()
		if !sleep(ctx, 5*time.Second) {
			return
		}
	}
}

func (manager *discoveryManager) applyAvahiLine(line string) {
	fields := strings.Split(line, ";")
	if len(fields) < 4 {
		return
	}
	instance := avahiUnescape(fields[3])
	if fields[0] == "-" {
		manager.mu.Lock()
		for id, host := range manager.hosts {
			if host.Instance == instance {
				delete(manager.hosts, id)
			}
		}
		manager.mu.Unlock()
		return
	}
	if fields[0] != "=" || len(fields) < 10 {
		return
	}
	values := parseAvahiText(fields[9])
	if values["id"] == "" || values["protocol"] != "2" {
		return
	}
	port, err := strconv.Atoi(fields[8])
	if err != nil {
		return
	}
	tlsPort, err := strconv.Atoi(values["tlsPort"])
	if err != nil {
		return
	}
	name := values["name"]
	if name == "" {
		name = instance
	}
	host := discoveredHost{
		ID: values["id"], Name: name, Instance: instance,
		Hostname: avahiUnescape(fields[6]), Address: fields[7], Port: port, TLSPort: tlsPort,
		LastSeen: time.Now(),
	}
	manager.mu.Lock()
	manager.hosts[host.ID] = host
	manager.mu.Unlock()
}

func parseAvahiText(text string) map[string]string {
	values := make(map[string]string)
	parts := strings.Split(text, `"`)
	for index := 1; index < len(parts); index += 2 {
		key, value, found := strings.Cut(avahiUnescape(parts[index]), "=")
		if found {
			values[key] = value
		}
	}
	return values
}

func avahiUnescape(value string) string {
	var result strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' || index+1 >= len(value) {
			result.WriteByte(value[index])
			continue
		}
		if index+3 < len(value) {
			if number, err := strconv.Atoi(value[index+1 : index+4]); err == nil {
				result.WriteByte(byte(number))
				index += 3
				continue
			}
		}
		index++
		result.WriteByte(value[index])
	}
	return result.String()
}

func (manager *discoveryManager) collect(ctx context.Context, entries <-chan *zeroconf.ServiceEntry) {
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-entries:
			if !ok {
				return
			}
			host, ok := discoveredHostFromEntry(entry)
			if !ok {
				continue
			}
			manager.mu.Lock()
			manager.hosts[host.ID] = host
			manager.mu.Unlock()
		}
	}
}

func (manager *discoveryManager) list() []discoveredHost {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	hosts := make([]discoveredHost, 0, len(manager.hosts))
	for _, host := range manager.hosts {
		hosts = append(hosts, host)
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Name < hosts[j].Name })
	return hosts
}

func (manager *discoveryManager) get(id string) (discoveredHost, bool) {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	host, ok := manager.hosts[id]
	return host, ok
}

func (manager *discoveryManager) listReachable(ctx context.Context) []discoveredHost {
	hosts := manager.list()
	client := &http.Client{Timeout: 750 * time.Millisecond}
	reachable := make([]discoveredHost, 0, len(hosts))
	for _, host := range hosts {
		healthHost := host.Hostname
		if host.Address != "" {
			healthHost = host.Address
		}
		healthURL := "http://" + net.JoinHostPort(healthHost, strconv.Itoa(host.Port)) + "/healthz"
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			continue
		}
		response, err := client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				reachable = append(reachable, host)
			}
		}
	}
	return reachable
}

func (manager *discoveryManager) allowsOrigin(origin string) bool {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	for _, host := range manager.hosts {
		if host.dashboardURL() == origin {
			return true
		}
	}
	return false
}

func discoveredHostFromEntry(entry *zeroconf.ServiceEntry) (discoveredHost, bool) {
	values := make(map[string]string)
	for _, field := range entry.Text {
		key, value, found := strings.Cut(field, "=")
		if found {
			values[key] = value
		}
	}
	if values["id"] == "" || values["protocol"] != "2" {
		return discoveredHost{}, false
	}
	address := ""
	if len(entry.AddrIPv4) > 0 {
		address = entry.AddrIPv4[0].String()
	} else if len(entry.AddrIPv6) > 0 {
		address = entry.AddrIPv6[0].String()
	}
	hostname := strings.TrimSuffix(entry.HostName, ".")
	if hostname == "" && address == "" {
		return discoveredHost{}, false
	}
	name := values["name"]
	if name == "" {
		name = strings.TrimSuffix(entry.Instance, ".")
	}
	tlsPort, err := strconv.Atoi(values["tlsPort"])
	if err != nil {
		return discoveredHost{}, false
	}
	return discoveredHost{
		ID: values["id"], Name: name, Instance: strings.TrimSuffix(entry.Instance, "."),
		Hostname: hostname, Address: address,
		Port: entry.Port, TLSPort: tlsPort, LastSeen: time.Now(),
	}, true
}

func (host discoveredHost) dashboardURL() string {
	name := host.Hostname
	if name == "" {
		name = host.Address
	}
	return "http://" + net.JoinHostPort(name, strconv.Itoa(host.Port))
}

func foremanPort(address string) int {
	_, portString, err := net.SplitHostPort(address)
	if err == nil {
		if port, parseErr := strconv.Atoi(portString); parseErr == nil {
			return port
		}
	}
	return 4040
}

func logDiscoveryError(err error) {
	if err != nil && !discoveryDisabled() {
		log.Printf("Foreman discovery unavailable: %v", err)
	}
}

func discoveryDisabled() bool {
	disabled, _ := strconv.ParseBool(os.Getenv("FOREMAN_DISABLE_DISCOVERY"))
	return disabled
}
