#!/bin/bash

# Define terminal colors
GREEN='\033[0;32m'
BLUE='\033[0;34m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Check if .env file exists
if [ ! -f ".env" ]; then
    echo -e "${RED}⚠️  Warning: .env configuration file not found!${NC}"
    echo -e "Creating one from .env.example..."
    if [ -f ".env.example" ]; then
        cp .env.example .env
        echo -e "${YELLOW}ℹ️  Created .env file. Please edit it to include your Cloudflare R2 credentials.${NC}"
    else
        echo -e "${RED}Error: .env.example file not found. Cannot auto-create .env.${NC}"
    fi
fi

# Determine running mode
if [ "$1" == "--prod" ]; then
    # Production mode: run the compiled binary
    if [ ! -f "./apsthira" ]; then
        echo -e "${YELLOW}ℹ️  Compiled binary './apsthira' not found. Compiling first...${NC}"
        ./build.sh
    fi
    echo -e "${BLUE}🚀 Starting Apsthira (Production Mode)...${NC}"
    ./apsthira
else
    # Development mode: compile and run on-the-fly
    echo -e "${BLUE}🚀 Starting Apsthira (Development Mode)...${NC}"
    go run .
fi
