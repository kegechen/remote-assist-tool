#!/bin/bash

echo "Building Remote Assist Tool..."

mkdir -p bin

echo "Building relay server..."
go build -o bin/relay ./cmd/relay
if [ $? -ne 0 ]; then
    echo "Failed to build relay"
    exit 1
fi

echo "Building remote client..."
go build -o bin/remote ./cmd/remote
if [ $? -ne 0 ]; then
    echo "Failed to build remote"
    exit 1
fi

echo "Build complete!"
echo "Binaries in: $(pwd)/bin"
