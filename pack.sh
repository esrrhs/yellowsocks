#! /bin/bash
NAME="yellowsocks"

rm pack -rf
rm pack.zip -f
mkdir -p pack

targets=(
  "linux/amd64/yellowsocks-cli"
  "linux/arm64/yellowsocks-cli"
  "windows/amd64/yellowsocks-gui.exe"
  "windows/arm64/yellowsocks-gui.exe"
)

for target in "${targets[@]}"; do
  os=$(echo "$target" | cut -d'/' -f1)
  arch=$(echo "$target" | cut -d'/' -f2)
  bin_name=$(echo "$target" | cut -d'/' -f3)

  echo "==> Building $os/$arch: $bin_name"

  if [ "$os" == "windows" ]; then
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -ldflags="-s -w" -o "$bin_name" ./cmd/yellowsocks-gui
  else
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -ldflags="-s -w" -o "$bin_name" ./cmd/yellowsocks-cli
  fi

  if [ $? -ne 0 ]; then
    echo "Build failed for $os/$arch"
    exit 1
  fi

  zip_file="${NAME}_${os}_${arch}.zip"
  zip "$zip_file" "$bin_name"
  mv "$zip_file" pack/
  rm -f "$bin_name"
done

zip pack.zip pack/ -r
echo "All packages built successfully in pack/ and pack.zip"
