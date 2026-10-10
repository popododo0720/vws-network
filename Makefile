.PHONY: test lint

test:
	CGO_ENABLED=0 go test ./...

lint:
	golangci-lint run
