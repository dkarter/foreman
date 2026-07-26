# Foreman Security

## Security model

Foreman assumes the Mac and kiosk may share an untrusted local network. Pairing requires a person at both devices, and all dashboard and resource-reporter traffic after pairing is encrypted with TLS 1.3 and authenticated with a device credential.

The Mac creates a persistent self-signed P-256 TLS identity on first launch. Its exact leaf-certificate SHA-256 fingerprint is included in the credential encrypted by the pairing exchange. The kiosk accepts TLS only when the presented leaf certificate matches that fingerprint.

## Pairing

1. The Mac explicitly enables pairing for three minutes.
2. The kiosk and Mac perform ephemeral P-256 ECDH over the bootstrap HTTP connection.
3. Both derive and display the same six-digit short authentication string.
4. A person confirms the code on both devices.
5. The Mac encrypts a random 256-bit device secret, host identity, transport version, and TLS certificate fingerprint with AES-256-GCM under the ECDH-derived key.
6. The kiosk stores the credential and uses its certificate fingerprint for every subsequent TLS connection.

Comparing the code is essential. An active attacker controlling the bootstrap connection derives a different code on each side and is detected when the displayed values do not match.

## Runtime transport

Chromium communicates only with the kiosk controller on `127.0.0.1:4041`. The controller proxies dashboard WebSockets to the selected Mac over certificate-pinned WSS. The resource reporter connects independently over the same pinned WSS transport.

Every WSS upgrade also carries a device ID, timestamp, random nonce, and HMAC-SHA256 signature. The Mac rejects expired timestamps, reused nonces, unknown devices, and invalid signatures. TLS protects WebSocket frames from observation and modification in transit.

The Mac's dashboard remains available over loopback HTTP for the native menu-bar app. Non-loopback dashboard and satellite WebSockets require TLS.

## Plaintext metadata

Bonjour discovery records are public LAN metadata. They contain the stable host ID, display name, bootstrap port, TLS port, and protocol version, but no credentials or authoritative certificate pin.

Initial pairing API requests use HTTP because the kiosk does not trust the Mac's certificate until pairing completes. Pairing secrets and the certificate fingerprint are protected by ECDH, AES-GCM, and the user-verified code.

## Stored secrets

- The Mac pairing store and TLS private key are written atomically with mode `0600` under `~/.config/foreman` by default.
- The kiosk's active reporter credential is written atomically with mode `0600`.
- Chromium stores one origin-scoped credential per paired Mac in local storage on the kiosk.

Anyone who can execute code as the corresponding OS user or inject JavaScript into the trusted dashboard assets can access those credentials. Foreman does not attempt to defend against a fully compromised endpoint.

## Revocation and identity changes

Revoking a kiosk removes its device secret and disconnects its active dashboard and reporter sessions. Future authentication attempts fail.

The Mac TLS certificate is intentionally long-lived and is not silently rotated. If a paired Mac presents another certificate, the kiosk fails closed and retains the old credential for diagnosis. Recovery requires explicitly forgetting and pairing again.

Deleting or corrupting a TLS identity after TLS-capable kiosks have paired causes the Mac service to fail at startup rather than generating an unauthenticated replacement.

## Out of scope

Foreman does not protect against:

- Compromise of the Mac, kiosk, browser profile, or their OS user accounts.
- A user approving pairing when the displayed codes differ.
- Denial of service through packet blocking, mDNS interference, or repeated connection attempts.
- Exposure of discovery metadata and network endpoints.

## Reporting vulnerabilities

Do not open a public issue for a vulnerability that exposes credentials or enables unauthorized control. Contact the project maintainer privately with reproduction steps, affected versions, and the expected impact.
