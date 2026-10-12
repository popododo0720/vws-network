//go:build envtest

package network

import (
	"context"
	"testing"
)

func TestEnvtestReadyReadsExternalSubnet(t *testing.T) {
	_, c := startEnv(t)
	ctx := context.Background()
	k := envKube(c, false)
	k.externalSubnet = "ready-ext"
	if err := k.ready(ctx); err == nil {
		t.Fatal("ready with no external subnet = nil")
	}
	seed(t, c, gvkSubnet, "", "ready-ext", "", map[string]any{"cidrBlock": "203.0.113.0/24"}, nil)
	if err := k.ready(ctx); err != nil {
		t.Fatalf("ready = %v", err)
	}
}
