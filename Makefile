.PHONY: test envtest lint

test:
	CGO_ENABLED=0 go test ./...

# Needs setup-envtest v0.25.2: KUBEBUILDER_ASSETS=$(setup-envtest use 1.37.0 -p path)
envtest:
	go test -tags envtest -count=1 ./internal/network

lint:
	golangci-lint run
