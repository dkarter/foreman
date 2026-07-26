package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

const pairingLifetime = 3 * time.Minute

var rawBase64 = base64.RawURLEncoding

type pairedDevice struct {
	TransportVersion int       `json:"transportVersion"`
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Secret           string    `json:"secret"`
	PairedAt         time.Time `json:"pairedAt"`
	HostID           string    `json:"hostId,omitempty"`
	HostName         string    `json:"hostName,omitempty"`
	Endpoint         string    `json:"endpoint,omitempty"`
	TLSCertSHA256    string    `json:"tlsCertSha256"`
}

type pairingStore struct {
	HostID   string         `json:"hostId"`
	HostName string         `json:"hostName"`
	Devices  []pairedDevice `json:"devices"`
}

type pendingPairing struct {
	ID             string
	Name           string
	Code           string
	RemoteAddress  string
	EncryptionKey  [32]byte
	ExpiresAt      time.Time
	KioskApproved  bool
	HostApproved   bool
	EncryptedReply *pairingCredential
}

type pairingCredential struct {
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

type pairingManager struct {
	mu             sync.Mutex
	path           string
	hostID         string
	hostName       string
	devices        map[string]pairedDevice
	pending        map[string]*pendingPairing
	nonces         map[string]time.Time
	enabledUntil   time.Time
	tlsFingerprint string
	tlsPort        int
}

func newPairingManager(path string) (*pairingManager, error) {
	manager := &pairingManager{
		path:    path,
		devices: make(map[string]pairedDevice),
		pending: make(map[string]*pendingPairing),
		nonces:  make(map[string]time.Time),
	}
	data, err := os.ReadFile(path)
	if err == nil {
		var store pairingStore
		if err := json.Unmarshal(data, &store); err != nil {
			return nil, fmt.Errorf("read pairing store: %w", err)
		}
		manager.hostID = store.HostID
		manager.hostName = store.HostName
		for _, device := range store.Devices {
			secret, decodeErr := rawBase64.DecodeString(device.Secret)
			if decodeErr != nil || device.ID == "" || len(secret) != 32 {
				return nil, errors.New("pairing store contains an invalid device credential")
			}
			manager.devices[device.ID] = device
		}
	} else if path != "" && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read pairing store: %w", err)
	}
	if manager.hostID == "" {
		manager.hostID, err = randomValue(18)
		if err != nil {
			return nil, err
		}
	}
	if manager.hostName == "" {
		hostname, _ := os.Hostname()
		manager.hostName = hostname
	}
	if err := manager.saveLocked(); err != nil {
		return nil, fmt.Errorf("save pairing store: %w", err)
	}
	return manager, nil
}

func (manager *pairingManager) begin(name, encodedClientKey, remoteAddress string) (*pendingPairing, string, error) {
	clientKeyBytes, err := rawBase64.DecodeString(encodedClientKey)
	if err != nil {
		return nil, "", errors.New("invalid client key")
	}
	clientKey, err := ecdh.P256().NewPublicKey(clientKeyBytes)
	if err != nil {
		return nil, "", errors.New("invalid client key")
	}
	serverKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	sharedSecret, err := serverKey.ECDH(clientKey)
	if err != nil {
		return nil, "", err
	}
	serverPublicKey := serverKey.PublicKey().Bytes()
	keyMaterial := sha256.Sum256(append(append(sharedSecret, clientKeyBytes...), serverPublicKey...))
	codeMAC := hmac.New(sha256.New, sharedSecret)
	codeMAC.Write([]byte("foreman-pairing-code"))
	codeMAC.Write(clientKeyBytes)
	codeMAC.Write(serverPublicKey)
	code := fmt.Sprintf("%06d", binary.BigEndian.Uint32(codeMAC.Sum(nil)[:4])%1000000)
	id, err := randomValue(24)
	if err != nil {
		return nil, "", err
	}
	if name == "" {
		name = "Foreman kiosk"
	}
	remoteHost, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		remoteHost = remoteAddress
	}
	pending := &pendingPairing{
		ID:            id,
		Name:          name,
		Code:          code,
		RemoteAddress: remoteHost,
		EncryptionKey: keyMaterial,
		ExpiresAt:     time.Now().Add(pairingLifetime),
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.expireLocked()
	if time.Now().After(manager.enabledUntil) {
		return nil, "", errors.New("enable pairing from the Foreman menu on the Mac")
	}
	for id, existing := range manager.pending {
		if existing.RemoteAddress != remoteHost {
			return nil, "", errors.New("another pairing request is pending")
		}
		delete(manager.pending, id)
	}
	manager.pending[id] = pending
	return pending, rawBase64.EncodeToString(serverPublicKey), nil
}

func (manager *pairingManager) confirm(id string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.expireLocked()
	pending := manager.pending[id]
	if pending == nil {
		return errors.New("pairing request is not available")
	}
	pending.KioskApproved = true
	return nil
}

func (manager *pairingManager) decide(id string, approve bool) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.expireLocked()
	pending := manager.pending[id]
	if pending == nil {
		return errors.New("pairing request is not available")
	}
	if !approve {
		delete(manager.pending, id)
		return nil
	}
	pending.HostApproved = true
	return nil
}

func (manager *pairingManager) status(id string) (string, *pairingCredential, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.expireLocked()
	pending := manager.pending[id]
	if pending == nil {
		return "expired", nil, nil
	}
	if !pending.KioskApproved || !pending.HostApproved {
		return "pending", nil, nil
	}
	if pending.EncryptedReply == nil {
		deviceID, err := randomValue(18)
		if err != nil {
			return "", nil, err
		}
		secret, err := randomValue(32)
		if err != nil {
			return "", nil, err
		}
		device := &pairedDevice{
			TransportVersion: 2, ID: deviceID, Name: pending.Name, Secret: secret,
			PairedAt: time.Now().UTC(), HostID: manager.hostID, HostName: manager.hostName,
			TLSCertSHA256: manager.tlsFingerprint,
		}
		manager.devices[device.ID] = *device
		if err := manager.saveLocked(); err != nil {
			return "", nil, err
		}
		plaintext, _ := json.Marshal(device)
		reply, err := encryptCredential(pending.EncryptionKey[:], plaintext)
		if err != nil {
			return "", nil, err
		}
		pending.EncryptedReply = reply
	}
	return "paired", pending.EncryptedReply, nil
}

func encryptCredential(key, plaintext []byte) (*pairingCredential, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return &pairingCredential{
		Nonce:      rawBase64.EncodeToString(nonce),
		Ciphertext: rawBase64.EncodeToString(aead.Seal(nil, nonce, plaintext, nil)),
	}, nil
}

func (manager *pairingManager) pendingState() map[string]any {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.expireLocked()
	devices := make([]map[string]any, 0, len(manager.devices))
	for _, device := range manager.devices {
		devices = append(devices, map[string]any{
			"id": device.ID, "name": device.Name, "pairedAt": device.PairedAt,
		})
	}
	result := map[string]any{
		"hostId": manager.hostID, "hostName": manager.hostName, "devices": devices,
		"pairingEnabled": time.Now().Before(manager.enabledUntil), "pairingEnabledUntil": manager.enabledUntil,
		"discoveryEnabled": !discoveryDisabled(),
	}
	for _, pending := range manager.pending {
		if pending.EncryptedReply != nil {
			continue
		}
		result["pending"] = map[string]any{
			"id": pending.ID, "name": pending.Name, "code": pending.Code,
			"remoteAddress": pending.RemoteAddress, "kioskApproved": pending.KioskApproved,
			"expiresAt": pending.ExpiresAt,
		}
		break
	}
	return result
}

func (manager *pairingManager) enable() time.Time {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.enabledUntil = time.Now().Add(pairingLifetime)
	return manager.enabledUntil
}

func (manager *pairingManager) hasDevice(deviceID string) bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	_, exists := manager.devices[deviceID]
	return exists
}

func (manager *pairingManager) unpair(deviceID string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if deviceID == "" {
		previous := manager.devices
		manager.devices = make(map[string]pairedDevice)
		if err := manager.saveLocked(); err != nil {
			manager.devices = previous
			return err
		}
	} else {
		previous, exists := manager.devices[deviceID]
		delete(manager.devices, deviceID)
		if err := manager.saveLocked(); err != nil {
			if exists {
				manager.devices[deviceID] = previous
			}
			return err
		}
	}
	manager.nonces = make(map[string]time.Time)
	return nil
}

func (manager *pairingManager) complete(id string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	delete(manager.pending, id)
}

func (manager *pairingManager) removeLegacyDevices() error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	changed := false
	for id, device := range manager.devices {
		if device.TransportVersion < 2 {
			delete(manager.devices, id)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	manager.nonces = make(map[string]time.Time)
	return manager.saveLocked()
}

func (manager *pairingManager) authorize(r *http.Request, path string) bool {
	deviceID := r.URL.Query().Get("device")
	timestamp := r.URL.Query().Get("timestamp")
	nonce := r.URL.Query().Get("nonce")
	providedSignature, err := rawBase64.DecodeString(r.URL.Query().Get("signature"))
	if err != nil || nonce == "" {
		return false
	}
	unixTime, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || time.Since(time.Unix(unixTime, 0)).Abs() > 30*time.Second {
		return false
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.expireNoncesLocked()
	device, exists := manager.devices[deviceID]
	if !exists {
		return false
	}
	if _, used := manager.nonces[nonce]; used {
		return false
	}
	secret, err := rawBase64.DecodeString(device.Secret)
	if err != nil {
		return false
	}
	expected := authSignature(secret, r.Method, path, deviceID, timestamp, nonce)
	if subtle.ConstantTimeCompare(providedSignature, expected) != 1 {
		return false
	}
	manager.nonces[nonce] = time.Now().Add(time.Minute)
	return true
}

func authSignature(secret []byte, method, path, deviceID, timestamp, nonce string) []byte {
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%s\n%s\n%s\n%s\n%s", method, path, deviceID, timestamp, nonce)
	return mac.Sum(nil)
}

func (manager *pairingManager) saveLocked() error {
	if manager.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(manager.path), 0o700); err != nil {
		return err
	}
	devices := make([]pairedDevice, 0, len(manager.devices))
	for _, device := range manager.devices {
		devices = append(devices, device)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].Name < devices[j].Name })
	data, err := json.MarshalIndent(pairingStore{
		HostID: manager.hostID, HostName: manager.hostName, Devices: devices,
	}, "", "  ")
	if err != nil {
		return err
	}
	return savePrivateFile(manager.path, append(data, '\n'))
}

func savePrivateFile(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".foreman-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func (manager *pairingManager) expireLocked() {
	now := time.Now()
	for id, pending := range manager.pending {
		if now.After(pending.ExpiresAt) {
			delete(manager.pending, id)
		}
	}
	manager.expireNoncesLocked()
}

func (manager *pairingManager) expireNoncesLocked() {
	now := time.Now()
	for nonce, expires := range manager.nonces {
		if now.After(expires) {
			delete(manager.nonces, nonce)
		}
	}
}

func randomValue(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return rawBase64.EncodeToString(value), nil
}

func pairingPath() string {
	if configured := os.Getenv("FOREMAN_PAIRING_PATH"); configured != "" {
		return configured
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "foreman", "paired-device.json")
}
