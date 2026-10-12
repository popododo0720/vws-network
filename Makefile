.PHONY: test envtest lint

test:
	CGO_ENABLED=0 go test ./...

# Needs setup-envtest: KUBEBUILDER_ASSETS=$(setup-envtest use -p path)
envtest:
	go test -tags envtest -count=1 ./internal/network

lint:
	golangci-lint run
