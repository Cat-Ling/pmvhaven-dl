#!/bin/bash
echo "Building"
CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o pmvhaven-dl-exp main.go
echo "Build complete! Output: pmvhaven-dl-exp"
