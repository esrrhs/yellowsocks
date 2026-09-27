FROM golang:1.26 AS build-env

WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /yellowsocks-cli ./cmd/yellowsocks-cli

FROM debian:stable-slim
COPY --from=build-env /yellowsocks-cli /usr/local/bin/yellowsocks-cli
ENTRYPOINT ["yellowsocks-cli"]
