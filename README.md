# Foreman

Foreman is a lightweight Raspberry Pi touchscreen interface that monitors live Herdr agents and focuses their panes with a tap.

The kiosk UI uses TypeScript and Preact, bundled into the Go binary with esbuild.

## Install

- `mise run install-host` installs the Go service and native macOS menu-bar controller.
- `mise run install-kiosk` installs the Chromium autostart entry, desktop launcher, and Raspberry Pi resource reporter.
- `mise run vnc-open` opens the Pi display through its local-only VNC server and an SSH tunnel.

The menu-bar app and kiosk settings page share the resource polling interval. Five seconds is the default; 10, 30, and 60-second intervals are also available. Compact mode fits up to 15 agent tiles at 800×480.

## Package build approval

`esbuild@0.28.1` is allowed to run its postinstall because it selects and verifies the platform binary. The reviewed release is OIDC trusted-published with SLSA provenance and a registry signature; its npm `gitHead` matches upstream tag `v0.28.1`, the tarball matches registry integrity, and `aube audit` reports no advisories.
