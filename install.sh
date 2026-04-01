#!/usr/bin/env bash
set -e

REPO="github.com/abarrell/compose-logs"
BIN="compose-logs"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

info()  { echo -e "${GREEN}==>${NC} $*"; }
warn()  { echo -e "${YELLOW}warning:${NC} $*"; }
error() { echo -e "${RED}error:${NC} $*" >&2; exit 1; }

command -v go &>/dev/null || error "Go is required. Install it from https://go.dev/dl/ and retry."

info "Installing ${BIN}..."
go install "${REPO}@latest"

GOBIN="$(go env GOPATH)/bin"
if [[ ":$PATH:" != *":${GOBIN}:"* ]]; then
    warn "${GOBIN} is not in your PATH."
    warn "Add this to your shell profile and restart your terminal:"
    warn "  export PATH=\"\$PATH:${GOBIN}\""
fi

info "Done! Run '${BIN}' to get started."
