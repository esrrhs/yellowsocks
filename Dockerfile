FROM golang AS build-env

RUN GO111MODULE=off go get -u github.com/esrrhs/yellowsocks
RUN GO111MODULE=off go get -u github.com/esrrhs/yellowsocks/...
RUN GO111MODULE=off go install github.com/esrrhs/yellowsocks

FROM debian
COPY --from=build-env /go/bin/yellowsocks .
WORKDIR ./
