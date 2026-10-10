#!/bin/sh
set -eu
# The executable and embedded Web are the same program used in terminal mode.
cd "$(dirname "$0")/.."
output_dir="build/Brodge.app"
check_stopped() {
 if [ -f "$output_dir/Contents/MacOS/Brodge" ] && lsof -t "$output_dir/Contents/MacOS/Brodge" >/dev/null 2>&1; then
  printf '%s\n' 'Brodge is running. Close it from Web before replacing the application.' >&2
  exit 1
 fi
}
check_stopped
mkdir -p build
package_work=$(mktemp -d build/.brodge-package.XXXXXX)
cleanup() {
 if [ ! -d "$output_dir" ] && [ -d "$package_work/previous.app" ]; then
  mv "$package_work/previous.app" "$output_dir"
 fi
 rm -rf "$package_work"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
app_dir="$package_work/Brodge.app"
mkdir -p "$app_dir/Contents/MacOS" "$app_dir/Contents/Resources"
icon_work="$package_work/icons"
iconset="$icon_work/Brodge.iconset"
mkdir -p "$iconset"
for size in 16 32 128 256 512; do
 sips -z "$size" "$size" web/public/assets/brodge-icon.png --out "$iconset/icon_${size}x${size}.png" >/dev/null
 double_size=$((size * 2))
 sips -z "$double_size" "$double_size" web/public/assets/brodge-icon.png --out "$iconset/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$iconset" -o "$app_dir/Contents/Resources/Brodge.icns"
cp Brodge "$app_dir/Contents/MacOS/Brodge"
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
fi
./scripts/sign-macos.sh "$app_dir"
# After migrating from ad hoc, refuse accidental signing-identity changes.
if [ -d "$output_dir" ]; then
 old_requirement=$(codesign -d -r- "$output_dir" 2>/dev/null) || exit 1
 case "$old_requirement" in
  'designated => '*) codesign --verify --strict -R "=${old_requirement#designated => }" "$app_dir" ;;
  '# designated => cdhash '*) ;; # One-time migration from the old development signature.
  *) printf '%s\n' 'Previous Brodge signing identity could not be verified.' >&2; exit 1 ;;
 esac
fi
check_stopped
if [ -d "$output_dir" ]; then mv "$output_dir" "$package_work/previous.app"; fi
if ! mv "$app_dir" "$output_dir"; then
 if [ -d "$package_work/previous.app" ]; then mv "$package_work/previous.app" "$output_dir"; fi
 exit 1
fi
# Let Finder invalidate the cached icon when updating an existing bundle.
touch "$output_dir"
