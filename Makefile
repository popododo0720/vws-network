.PHONY: test envtest lint guard

test:
	CGO_ENABLED=1 go test -race ./...

# Needs setup-envtest v0.25.2: KUBEBUILDER_ASSETS=$(setup-envtest use 1.37.0 -p path)
envtest:
	CGO_ENABLED=1 go test -race -tags envtest -count=1 ./internal/network

lint:
	golangci-lint run

guard:
	scripts/guard-agents.sh selftest
	scripts/guard-agents.sh agents
