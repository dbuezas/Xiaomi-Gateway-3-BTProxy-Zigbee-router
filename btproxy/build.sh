#!/bin/sh
# Build gw3-btproxy for the gateway (MIPS32 little endian, soft float) and for this machine, and bundle
# the gateway files into the Home Assistant integration, which installs them on the gateway itself.
# The build is reproducible (no timestamps), so the gateway only gets a new binary when the code changes.
set -e
cd "$(dirname "$0")"
VERSION=$(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' ../custom_components/gw3_btproxy/manifest.json)
LDFLAGS="-s -w -buildid= -X main.version=$VERSION"
GOOS=linux GOARCH=mipsle GOMIPS=softfloat CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "$LDFLAGS" -o gw3-btproxy-mipsle .
go build -trimpath -buildvcs=false -ldflags "$LDFLAGS" -o gw3-btproxy .
mkdir -p ../custom_components/gw3_btproxy/bin
cp gw3-btproxy-mipsle ../custom_components/gw3_btproxy/bin/gw3-btproxy
cp ../gateway/gw3-btproxy.sh ../custom_components/gw3_btproxy/bin/gw3-btproxy.sh
ls -la ../custom_components/gw3_btproxy/bin
