.PHONY: build test check run migrate
build:
	go build -trimpath -o blog ./cmd/blog
test:
	go test -race ./...
check:
	go vet ./...
run:
	go run ./cmd/blog serve
migrate:
	go run ./cmd/blog migrate
