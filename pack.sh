#! /bin/bash
NAME="yellowsocks"

rm pack -rf
rm pack.zip -f
mkdir -p pack

# Cross-compile targets (CGO_ENABLED=0, can run from any host)
cross_targets=(
  "linux/amd64/yellowsocks-cli"
  "linux/arm64/yellowsocks-cli"
  "linux/arm/yellowsocks-cli"
  "linux/mipsle/yellowsocks-cli"
  "linux/mips/yellowsocks-cli"
  "windows/amd64/yellowsocks-gui.exe"
  "windows/arm64/yellowsocks-gui.exe"
)

for target in "${cross_targets[@]}"; do
  os=$(echo "$target" | cut -d'/' -f1)
  arch=$(echo "$target" | cut -d'/' -f2)
  bin_name=$(echo "$target" | cut -d'/' -f3)

  echo "==> Building $os/$arch: $bin_name"

  extra_env=""
  if [ "$arch" == "arm" ]; then
    extra_env="GOARM=7"
  elif [ "$arch" == "mips" ] || [ "$arch" == "mipsle" ]; then
    extra_env="GOMIPS=softfloat"
  fi

  if [ "$os" == "windows" ]; then
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -ldflags="-s -w" -o "$bin_name" ./cmd/yellowsocks-gui
  else
    env CGO_ENABLED=0 GOOS=$os GOARCH=$arch $extra_env go build -ldflags="-s -w" -o "$bin_name" ./cmd/yellowsocks-cli
  fi

  if [ $? -ne 0 ]; then
    echo "Build failed for $os/$arch"
    exit 1
  fi

  zip_file="${NAME}_${os}_${arch}.zip"
  # Include OpenWrt integration scripts for linux targets
  if [ "$os" == "linux" ]; then
    zip -r "$zip_file" "$bin_name" openwrt config.example.yaml
  else
    zip "$zip_file" "$bin_name"
  fi
  mv "$zip_file" pack/
  rm -f "$bin_name"
done

# macOS GUI targets — MUST be built on a macOS host (CGO required for systray/Objective-C)
# Uncomment and run this section on macOS:
#
# darwin_arches=("amd64" "arm64")
# for arch in "${darwin_arches[@]}"; do
#   echo "==> Building darwin/$arch: yellowsocks-gui"
#   CGO_ENABLED=1 GOOS=darwin GOARCH=$arch go build -ldflags="-s -w" -o yellowsocks-gui ./cmd/yellowsocks-gui
#   if [ $? -ne 0 ]; then echo "Build failed for darwin/$arch"; exit 1; fi
#   zip_file="${NAME}_darwin_${arch}.zip"
#   zip "$zip_file" yellowsocks-gui
#   mv "$zip_file" pack/
#   rm -f yellowsocks-gui
# done

zip pack.zip pack/ -r
echo "All packages built successfully in pack/ and pack.zip"
