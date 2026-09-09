ARG GO_VERSION=1.26
ARG ALPINE_VERSION=3.21
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -ldflags "-s -w -X main.version=${VERSION}" -a -o node-disk-sentinel cmd/node-disk-sentinel/main.go

FROM alpine:${ALPINE_VERSION}

RUN apk add --no-cache smartmontools ca-certificates

WORKDIR /

COPY --from=builder /workspace/node-disk-sentinel /usr/local/bin/node-disk-sentinel

ENTRYPOINT ["node-disk-sentinel"]
