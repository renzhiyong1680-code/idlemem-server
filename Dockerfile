# syntax=docker/dockerfile:1
#
# XianCun Cloud control plane + agent images.
#
#   docker build --target server -t xiancun/server .
#   docker build --target agent  -t xiancun/agent  .
#   docker compose up -d          # server + one agent + volume ./data
#
# The agent binary is built from the idlemem-core module pinned in go.mod, so the
# agent version always matches what this server was tested against.

FROM golang:1.27-alpine AS build
ARG CORE_VERSION=v0.1.1
ENV CGO_ENABLED=0 \
    GOPROXY=https://goproxy.io,direct \
    GOSUMDB=off \
    GONOSUMDB=*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags "-s -w -X main.version=${CORE_VERSION}" -o /out/idlemem-server ./cmd/server
RUN GOFLAGS=-mod=mod go install github.com/renzhiyong1680-code/idlemem-core/cmd/xiancun-agent@${CORE_VERSION} \
 && cp /root/go/bin/xiancun-agent /out/xiancun-agent

FROM alpine:3.20 AS server
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/idlemem-server /usr/local/bin/idlemem-server
COPY scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh && mkdir -p /data
WORKDIR /data
EXPOSE 8080 6379
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]

FROM alpine:3.20 AS agent
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/xiancun-agent /usr/local/bin/xiancun-agent
COPY scripts/docker-agent-entrypoint.sh /usr/local/bin/docker-agent-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-agent-entrypoint.sh
WORKDIR /data
EXPOSE 16380
ENTRYPOINT ["/usr/local/bin/docker-agent-entrypoint.sh"]
