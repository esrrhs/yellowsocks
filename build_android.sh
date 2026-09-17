#!/bin/bash
set -e

if [ -z "$ANDROID_HOME" ]; then
    if [ -d "/usr/local/lib/android/sdk" ]; then
        export ANDROID_HOME="/usr/local/lib/android/sdk"
    elif [ -d "/root/android-sdk" ]; then
        export ANDROID_HOME="/root/android-sdk"
    fi
fi

if [ -z "$ANDROID_NDK_HOME" ] && [ -d "${ANDROID_HOME}/ndk" ]; then
    LATEST_NDK=$(ls -d "${ANDROID_HOME}/ndk"/* 2>/dev/null | sort -V | tail -n 1)
    if [ -n "$LATEST_NDK" ]; then
        export ANDROID_NDK_HOME="$LATEST_NDK"
    fi
fi

echo "--> ANDROID_HOME: ${ANDROID_HOME}"
echo "--> ANDROID_NDK_HOME: ${ANDROID_NDK_HOME}"

mkdir -p android/app/libs

echo "==> 2. Building yellowsocks.aar via gomobile..."
gomobile bind -target=android/arm64,android/arm,android/amd64 -androidapi 21 -o android/app/libs/yellowsocks.aar ./mobile

echo "==> 3. yellowsocks.aar successfully built in android/app/libs/:"
ls -lh android/app/libs/yellowsocks.aar

echo "==> Android library build completed successfully!"
