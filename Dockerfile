FROM golang:1.27.1-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/yellowsocks-cli ./cmd/yellowsocks-cli

FROM debian:bookworm-slim
WORKDIR /app
COPY --from=build /out/yellowsocks-cli ./yellowsocks-cli
ENTRYPOINT ["./yellowsocks-cli"]
