.PHONY: web test build run clean

web:
	cd web && npm ci && npm run build

test: web
	go test ./...

build: web
	go build -trimpath -ldflags "-s -w" -o bin/cf-r2-manager ./cmd/cf-r2-manager

run: web
	go run ./cmd/cf-r2-manager server --config ./config.example.yaml

clean:
	go clean

