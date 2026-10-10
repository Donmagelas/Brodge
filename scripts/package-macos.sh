#!/bin/sh
set -eu
# The executable and embedded Web are the same program used in terminal mode.
cd "$(dirname "$0")/.."
app_dir="build/Brodge.app"
mkdir -p "$app_dir/Contents/MacOS" "$app_dir/Contents/Resources"
icon_work="$(mktemp -d)"
trap 'rm -rf "$icon_work"' EXIT
iconset="$icon_work/Brodge.iconset"
mkdir "$iconset"
for size in 16 32 128 256 512; do
 sips -z "$size" "$size" web/public/assets/brodge-icon.png --out "$iconset/icon_${size}x${size}.png" >/dev/null
 double_size=$((size * 2))
 sips -z "$double_size" "$double_size" web/public/assets/brodge-icon.png --out "$iconset/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$iconset" -o "$app_dir/Contents/Resources/Brodge.icns"
cp Brodge "$app_dir/Contents/MacOS/Brodge"
rm -f "$app_dir/Contents/MacOS/BrodgeLauncher"
cat > "$app_dir/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>Brodge</string>
<key>CFBundleIdentifier</key><string>local.brodge.desktop</string>
<key>CFBundleName</key><string>Brodge</string>
<key>CFBundleDisplayName</key><string>Brodge</string>
<key>CFBundleIconFile</key><string>Brodge.icns</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>0.1.0</string>
<key>CFBundleVersion</key><string>1</string>
<key>LSUIElement</key><true/>
<key>LSMinimumSystemVersion</key><string>13.0</string>
<key>NSDocumentsFolderUsageDescription</key><string>Brodge needs access to the existing configuration and conversation files you selected.</string>
</dict></plist>
PLIST
if [ "$#" -eq 1 ]; then
 case "$1" in /*) printf '%s\n' "$1" > "$app_dir/Contents/Resources/config-path";; *) exit 2;; esac
elif [ -f "$app_dir/Contents/Resources/config-path" ]; then
 rm "$app_dir/Contents/Resources/config-path"
fi
# Ad hoc signing is enough for this local development package.
codesign --force --sign - "$app_dir"
# Let Finder invalidate the cached icon when updating an existing bundle.
touch "$app_dir"
