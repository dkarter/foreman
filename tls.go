package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const defaultTLSPort = 4042

type storedTLSIdentity struct {
	Certificate string `json:"certificate"`
	PrivateKey  string `json:"privateKey"`
}

type tlsIdentity struct {
	Certificate tls.Certificate
	Fingerprint string
}

func loadTLSIdentity(path, hostID, hostName string, requireExisting bool) (tlsIdentity, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if requireExisting {
			return tlsIdentity{}, errors.New("TLS identity is missing; revoke paired kiosks before resetting it")
		}
		return generateTLSIdentity(path, hostID, hostName)
	}
	if err != nil {
		return tlsIdentity{}, err
	}
	var stored storedTLSIdentity
	if err := json.Unmarshal(data, &stored); err != nil {
		return tlsIdentity{}, fmt.Errorf("read TLS identity: %w", err)
	}
	certificateDER, err := rawBase64.DecodeString(stored.Certificate)
	if err != nil {
		return tlsIdentity{}, errors.New("TLS identity contains an invalid certificate")
	}
	privateKeyDER, err := rawBase64.DecodeString(stored.PrivateKey)
	if err != nil {
		return tlsIdentity{}, errors.New("TLS identity contains an invalid private key")
	}
	certificate, err := x509.ParseCertificate(certificateDER)
	if err != nil {
		return tlsIdentity{}, err
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(privateKeyDER)
	if err != nil {
		return tlsIdentity{}, err
	}
	privateKey, ok := parsedKey.(*ecdsa.PrivateKey)
	if !ok || privateKey.Curve != elliptic.P256() || !privateKey.PublicKey.Equal(certificate.PublicKey) {
		return tlsIdentity{}, errors.New("TLS identity certificate and private key do not match")
	}
	if time.Now().Before(certificate.NotBefore) || time.Now().After(certificate.NotAfter) {
		return tlsIdentity{}, errors.New("TLS identity certificate is not currently valid")
	}
	return makeTLSIdentity(certificate, privateKey), nil
}

func generateTLSIdentity(path, hostID, hostName string) (tlsIdentity, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tlsIdentity{}, err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tlsIdentity{}, err
	}
	dnsName := hostName
	if dnsName != "" && dnsName[len(dnsName)-1] != '.' && filepath.Ext(dnsName) != ".local" {
		dnsName += ".local"
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Foreman " + hostID},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{hostName, dnsName, "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return tlsIdentity{}, err
	}
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return tlsIdentity{}, err
	}
	data, _ := json.MarshalIndent(storedTLSIdentity{
		Certificate: rawBase64.EncodeToString(certificateDER),
		PrivateKey:  rawBase64.EncodeToString(privateKeyDER),
	}, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return tlsIdentity{}, err
	}
	if err := savePrivateFile(path, append(data, '\n')); err != nil {
		return tlsIdentity{}, err
	}
	certificate, _ := x509.ParseCertificate(certificateDER)
	return makeTLSIdentity(certificate, privateKey), nil
}

func makeTLSIdentity(certificate *x509.Certificate, privateKey *ecdsa.PrivateKey) tlsIdentity {
	fingerprint := sha256.Sum256(certificate.Raw)
	return tlsIdentity{
		Certificate: tls.Certificate{
			Certificate: [][]byte{certificate.Raw}, PrivateKey: privateKey, Leaf: certificate,
		},
		Fingerprint: rawBase64.EncodeToString(fingerprint[:]),
	}
}

func pinnedHTTPClient(encodedFingerprint string) (*http.Client, error) {
	expected, err := rawBase64.DecodeString(encodedFingerprint)
	if err != nil || len(expected) != sha256.Size {
		return nil, errors.New("invalid TLS certificate fingerprint")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, // Exact pin verification replaces Web PKI.
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("TLS peer sent no certificate")
			}
			actual := sha256.Sum256(state.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(actual[:], expected) != 1 {
				return errors.New("Foreman TLS identity does not match paired host")
			}
			return nil
		},
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}, nil
}

func tlsIdentityPath() string {
	if configured := os.Getenv("FOREMAN_TLS_IDENTITY_PATH"); configured != "" {
		return configured
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "foreman", "tls-identity.json")
}
