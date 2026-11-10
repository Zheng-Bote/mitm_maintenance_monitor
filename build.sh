#!/usr/bin/sh

MITM_VERSION=$(git describe --tags 2>/dev/null || echo "v0.1.0")

CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${MITM_VERSION}" -o ./bin/mitm-maintenance-monitor .

cp bin/mitm-maintenance-monitor ../../app/bin/.
