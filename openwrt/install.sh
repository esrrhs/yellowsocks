#!/bin/sh
set -e

echo "==> Installing YellowSocks on OpenWrt..."

DIR=$(cd "$(dirname "$0")" && pwd)

# 1. Check binary
if [ -f "$DIR/yellowsocks-cli" ]; then
    cp "$DIR/yellowsocks-cli" /usr/bin/yellowsocks-cli
    chmod +x /usr/bin/yellowsocks-cli
    echo "--> Copied binary to /usr/bin/yellowsocks-cli"
elif [ -f "/usr/bin/yellowsocks-cli" ]; then
    echo "--> Binary already exists in /usr/bin/yellowsocks-cli"
else
    echo "Warning: yellowsocks-cli binary not found in current folder."
    echo "Please place the compiled yellowsocks-cli binary into /usr/bin/."
fi

# 2. Config directory and sample config
mkdir -p /etc/yellowsocks
if [ ! -f /etc/yellowsocks/config.yaml ]; then
    if [ -f "$DIR/../config.example.yaml" ]; then
        cp "$DIR/../config.example.yaml" /etc/yellowsocks/config.yaml
    elif [ -f "$DIR/config.example.yaml" ]; then
        cp "$DIR/config.example.yaml" /etc/yellowsocks/config.yaml
    fi
    echo "--> Created default config at /etc/yellowsocks/config.yaml"
fi

# 3. UCI config
if [ -f "$DIR/etc/config/yellowsocks" ]; then
    cp "$DIR/etc/config/yellowsocks" /etc/config/yellowsocks
    echo "--> Installed UCI config to /etc/config/yellowsocks"
fi

# 4. Init.d procd script
if [ -f "$DIR/etc/init.d/yellowsocks" ]; then
    cp "$DIR/etc/init.d/yellowsocks" /etc/init.d/yellowsocks
    chmod +x /etc/init.d/yellowsocks
    /etc/init.d/yellowsocks enable
    echo "--> Enabled YellowSocks service (/etc/init.d/yellowsocks enable)"
fi

echo ""
echo "==> Installation complete!"
echo "Next steps:"
echo "1. Edit your server configuration:  vi /etc/yellowsocks/config.yaml"
echo "2. Enable the service:             uci set yellowsocks.main.enabled='1' && uci commit yellowsocks"
echo "3. Start the service:              /etc/init.d/yellowsocks start"
