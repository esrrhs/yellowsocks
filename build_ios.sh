#!/bin/bash
set -e

FRAMEWORK_NAME="YellowSocks"
OUTPUT_DIR="ios/Frameworks/${FRAMEWORK_NAME}.xcframework"

echo "==> 1. Checking Gomobile environment..."
if ! command -v gomobile &> /dev/null; then
    echo "gomobile not found. Installing gomobile..."
    go install golang.org/x/mobile/cmd/gomobile@latest
    go install golang.org/x/mobile/cmd/gobind@latest
    gomobile init
fi

mkdir -p ios/Frameworks

echo "==> 2. Building ${FRAMEWORK_NAME}.xcframework for iOS & iOS Simulator..."
# gomobile bind targets:
# ios: compiles for iOS physical device (arm64) and simulator (arm64, amd64)
gomobile bind -target=ios -bundleid=com.esrrhs.yellowsocks.core -o "${OUTPUT_DIR}" ./mobile

echo "==> 3. Framework successfully generated at:"
ls -ld "${OUTPUT_DIR}"

echo ""
echo "==> iOS & iPadOS framework build complete!"
echo "Next step: Open ios/YellowSocks.xcodeproj in Xcode and Run on your iPhone / iPad."
