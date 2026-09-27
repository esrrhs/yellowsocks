#! /bin/bash
NAME="yellowsocks"

rm -rf pack
rm -f pack.zip
mkdir -p pack

targets=(
  "linux/amd64"
  "linux/arm64"
  "linux/arm"
)

VERSION=$(cat VERSION 2>/dev/null || echo "1.0.0")
GIT_COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME=$(date -u '+%Y-%m-%d %H:%M:%S UTC')

echo "==> Packaging YellowSocks v${VERSION} (Commit: ${GIT_COMMIT}, Built: ${BUILD_TIME})"

LDFLAGS="-s -w -X github.com/esrrhs/yellowsocks/core/version.Version=${VERSION} -X github.com/esrrhs/yellowsocks/core/version.GitCommit=${GIT_COMMIT} -X \"github.com/esrrhs/yellowsocks/core/version.BuildTime=${BUILD_TIME}\""

for target in "${targets[@]}"; do
  os=$(echo "$target" | cut -d'/' -f1)
  arch=$(echo "$target" | cut -d'/' -f2)
  echo "==> Building $os/$arch"

  extra_env=""
  if [ "$arch" = "arm" ]; then
    extra_env="GOARM=7"
  fi

  env CGO_ENABLED=0 GOOS=$os GOARCH=$arch $extra_env go build -ldflags="$LDFLAGS" -o yellowsocks-cli ./cmd/yellowsocks-cli
  if [ $? -ne 0 ]; then
    echo "Build failed for $os/$arch"
    exit 1
  fi

  zip_file="${NAME}_${os}_${arch}.zip"
  zip "$zip_file" yellowsocks-cli config.example.yaml
  mv "$zip_file" pack/
  rm -f yellowsocks-cli
done

zip -r pack.zip pack/
echo "Linux packages are in pack/ and pack.zip"
