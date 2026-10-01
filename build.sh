#!/bin/sh
set -eu
go build -tags goolm -ldflags "-X main.Tag=$(git describe --exact-match --tags 2>/dev/null || true) -X main.Commit=$(git rev-parse HEAD) -X main.BuildTime=$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$@"
