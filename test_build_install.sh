#!/usr/bin/env bash

# Test
go test ./...

# Build
go build

# Install
cp wasmbrowsertest "$GOPATH/bin/wasmbrowsertest"
