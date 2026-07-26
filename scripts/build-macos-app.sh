#!/usr/bin/env bash
set -euo pipefail

version="${1:?usage: build-macos-app.sh VERSION}"
build_dir="$(mktemp -d)"
app_dir="dist/Foreman.app"
trap 'rm -rf "$build_dir"' EXIT

aube run build
rm -rf "$app_dir"
mkdir -p "$app_dir/Contents/MacOS" "$app_dir/Contents/Resources"

for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" go build -trimpath -ldflags='-s -w' \
    -o "$build_dir/foreman-server-$arch" .
done
lipo -create "$build_dir/foreman-server-amd64" "$build_dir/foreman-server-arm64" \
  -output "$app_dir/Contents/Resources/foreman-server"

swiftc -O -parse-as-library -target x86_64-apple-macos13.0 -framework AppKit \
  -framework Foundation macos/ForemanStatus.swift -o "$build_dir/foreman-status-amd64"
swiftc -O -parse-as-library -target arm64-apple-macos13.0 -framework AppKit \
  -framework Foundation macos/ForemanStatus.swift -o "$build_dir/foreman-status-arm64"
lipo -create "$build_dir/foreman-status-amd64" "$build_dir/foreman-status-arm64" \
  -output "$app_dir/Contents/MacOS/Foreman"

mkdir -p "$build_dir/Foreman.iconset"
sips -z 16 16 assets/foreman-app-icon.png --out "$build_dir/Foreman.iconset/icon_16x16.png" >/dev/null
sips -z 32 32 assets/foreman-app-icon.png --out "$build_dir/Foreman.iconset/icon_16x16@2x.png" >/dev/null
sips -z 32 32 assets/foreman-app-icon.png --out "$build_dir/Foreman.iconset/icon_32x32.png" >/dev/null
sips -z 64 64 assets/foreman-app-icon.png --out "$build_dir/Foreman.iconset/icon_32x32@2x.png" >/dev/null
sips -z 128 128 assets/foreman-app-icon.png --out "$build_dir/Foreman.iconset/icon_128x128.png" >/dev/null
sips -z 256 256 assets/foreman-app-icon.png --out "$build_dir/Foreman.iconset/icon_128x128@2x.png" >/dev/null
sips -z 256 256 assets/foreman-app-icon.png --out "$build_dir/Foreman.iconset/icon_256x256.png" >/dev/null
sips -z 512 512 assets/foreman-app-icon.png --out "$build_dir/Foreman.iconset/icon_256x256@2x.png" >/dev/null
sips -z 512 512 assets/foreman-app-icon.png --out "$build_dir/Foreman.iconset/icon_512x512.png" >/dev/null
cp assets/foreman-app-icon.png "$build_dir/Foreman.iconset/icon_512x512@2x.png"
iconutil -c icns "$build_dir/Foreman.iconset" -o "$app_dir/Contents/Resources/Foreman.icns"

cp macos/Info.plist "$app_dir/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $version" "$app_dir/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleVersion $version" "$app_dir/Contents/Info.plist"
cp assets/foreman-menubar-for-light.png assets/foreman-menubar-for-dark.png \
  web/assets/herdr.png "$app_dir/Contents/Resources/"

archive="dist/Foreman_${version}_macOS_universal.zip"
rm -f "$archive"
ditto -c -k --sequesterRsrc --keepParent "$app_dir" "$archive"
