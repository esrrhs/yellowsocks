#!/bin/bash
set -e

echo "==> 1. Setting up Android & Go Mobile Environment..."
export ANDROID_HOME=${ANDROID_HOME:-/root/android-sdk}
export ANDROID_NDK_HOME=${ANDROID_NDK_HOME:-/root/android-sdk/ndk/29.0.14206865}

mkdir -p android/app/libs

echo "==> 2. Building yellowsocks.aar via gomobile..."
gomobile bind -target=android/arm64,android/arm,android/amd64 -androidapi 21 -o android/app/libs/yellowsocks.aar ./mobile

echo "==> 3. yellowsocks.aar successfully built in android/app/libs/:"
ls -lh android/app/libs/yellowsocks.aar

echo "==> Android library build completed successfully!"
