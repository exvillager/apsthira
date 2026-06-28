#!/bin/bash

# Exit immediately if any command fails
set -e

# Define terminal colors
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${BLUE}🔨 Building Apsthira binary...${NC}"

# Check if Go is installed
if ! command -v go &> /dev/null; then
    echo "Error: Go is not installed. Please install Go before building."
    exit 1
fi

# Run tests (optional but good practice for open source)
echo -e "${YELLOW}🔍 Running go vet...${NC}"
go vet ./...

# Build the binary with size optimization flags
# -s: Omit symbols table (smaller binary size)
# -w: Omit DWARF debugging info (smaller binary size)
echo -e "${YELLOW}🚀 Compiling single binary...${NC}"
go build -ldflags="-s -w" -o apsthira .

# Print file details
if [ -f "./apsthira" ]; then
    BINARY_SIZE=$(du -h ./apsthira | cut -f1)
    echo -e "${GREEN}✓ Build succeeded!${NC}"
    echo -e "${GREEN}📦 Binary Location: ./apsthira${NC}"
    echo -e "${GREEN}⚖️ Binary Size: $BINARY_SIZE${NC}"
else
    echo "Error: Binary build succeeded but file not found."
    exit 1
fi
