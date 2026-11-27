.PHONY: verify run ping

# The gate: vet, then the tests.
verify:
	go vet ./...
	go test ./...

# Run it here, the world under ./data. Accepts Minecraft's EULA for you: read it first, https://aka.ms/MinecraftEULA
run:
	PORT=8080 DATA_DIR=./data EULA=true go run .

# Ask a server for its status through its websocket: make ping URL=wss://<address>/ws
URL ?= ws://localhost:8080/ws
ping:
	go run ./tools/mcping $(URL)
