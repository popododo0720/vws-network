.PHONY: test lint proto

test:
	CGO_ENABLED=0 go test ./...

lint:
	golangci-lint run

proto:
	protoc -I proto \
		--go_out=. --go_opt=module=github.com/popododo0720/vws-network \
		--go-grpc_out=. --go-grpc_opt=module=github.com/popododo0720/vws-network \
		proto/vws/network/v1/network.proto
