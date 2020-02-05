#!/usr/bin/env bash

# Test
go test ./...

# Build
go build

# Install
cp wasmbrowsertest "$GOPATH/bin/wasmbrowsertest"
cp wasmbrowsertest "/mnt/c/Users/gledr/Polyapp_Apps/gocode/bin/wasmbrowsertest"
