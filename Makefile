.PHONY: run test test-race test-integration check up down

run:
	go run ./cmd/server

test:
	go test ./...

test-race:
	go test -race ./...

test-integration:
	go test -race -tags=integration ./...

check:
	test -z "$$(gofmt -l cmd internal)"
	go vet ./...
	go test ./...
	go test -race ./...
	git diff --check

up:
	docker compose up --build

down:
	docker compose down
