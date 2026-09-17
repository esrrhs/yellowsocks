#!/bin/bash
set -e

APP_NAME="YellowSocks"
BIN_NAME="yellowsocks-gui"
APP_BUNDLE="${APP_NAME}.app"

echo "==> Building ${APP_NAME} for macOS..."

# Ensure CGO is enabled (systray and WebKit require native CGO)
export CGO_ENABLED=1

ARCH=$(uname -m)
if [ "$ARCH" = "x86_64" ]; then
    TARGET_ARCH="amd64"
elif [ "$ARCH" = "arm64" ]; then
    TARGET_ARCH="arm64"
else
    TARGET_ARCH="arm64"
fi

echo "--> Target architecture: ${TARGET_ARCH}"
GOOS=darwin GOARCH=${TARGET_ARCH} go build -ldflags="-s -w" -o "${BIN_NAME}" ./cmd/yellowsocks-gui

echo "==> Creating macOS App Bundle (${APP_BUNDLE})..."
rm -rf "${APP_BUNDLE}"
mkdir -p "${APP_BUNDLE}/Contents/MacOS"
mkdir -p "${APP_BUNDLE}/Contents/Resources"

mv "${BIN_NAME}" "${APP_BUNDLE}/Contents/MacOS/${APP_NAME}"
chmod +x "${APP_BUNDLE}/Contents/MacOS/${APP_NAME}"

# Generate Info.plist
cat << 'EOF' > "${APP_BUNDLE}/Contents/Info.plist"
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleExecutable</key>
    <string>YellowSocks</string>
    <key>CFBundleIdentifier</key>
    <string>com.esrrhs.yellowsocks</string>
    <key>CFBundleName</key>
    <string>YellowSocks</string>
    <key>CFBundlePackageType</key>
    <string>APPL</string>
    <key>CFBundleShortVersionString</key>
    <string>1.0.0</string>
    <key>CFBundleVersion</key>
    <string>1</string>
    <key>NSHighResolutionCapable</key>
    <true/>
    <key>NSRequiresAquaSystemAppearance</key>
    <false/>
</dict>
</plist>
EOF

echo "==> Done! You can run YellowSocks using:"
echo "    sudo open ${APP_BUNDLE}"
echo "  or directly from terminal:"
echo "    sudo ./${APP_BUNDLE}/Contents/MacOS/${APP_NAME} -config config.yaml"
