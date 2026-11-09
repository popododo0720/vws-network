package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/popododo0720/vws-lib/obs"

	"github.com/popododo0720/vws-network/internal/network"
)

// TestMetricsEndpointOptIn runs the binary's run with the vws-lib variables:
// OTEL_METRICS_EXPORTER=prometheus serves the service's vws_* series on
// /metrics at VWS_HEALTH_ADDR, without it /metrics is not served, and
// prometheus without a health address refuses start before the store opens.
func TestMetricsEndpointOptIn(t *testing.T) {
	t.Run("prometheus serves vws metrics", func(t *testing.T) {
		health, listen := freeAddr(t), freeAddr(t)
		setObsEnv(t, "prometheus", health)
		wait := startRun(t, listen)
		body := poll(t, "http://"+health+"/metrics", func(code int, body string) bool {
			return code == http.StatusOK && strings.Contains(body, "vws_rpc_server_requests_total")
		})
		for _, want := range []string{
			`vws_queue_depth{queue="network.operations"`,
			"vws_rpc_server_requests_total",
			"vws_build_info",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("/metrics lacks %s:\n%s", want, body)
			}
		}
		for _, line := range strings.Split(body, "\n") {
			if line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "vws_") {
				t.Errorf("/metrics serves a series outside vws_*: %s", line)
			}
		}
		wait()
	})
	t.Run("off by default", func(t *testing.T) {
		health, listen := freeAddr(t), freeAddr(t)
		setObsEnv(t, "", health)
		wait := startRun(t, listen)
		if code, body := get(t, "http://"+health+"/metrics"); code == http.StatusOK || strings.Contains(body, "vws_") {
			t.Fatalf("/metrics served without opt-in: %d %q", code, body)
		}
		wait()
	})
	t.Run("prometheus needs a health address", func(t *testing.T) {
		setObsEnv(t, "prometheus", "")
		db := filepath.Join(t.TempDir(), "net.db")
		err := run(t.Context(), freeAddr(t), db, network.StoreConfig{}, testPool, network.DefaultWorkers,
			network.KubeOVNConfig{})
		if err == nil || !strings.Contains(err.Error(), "obs setup") {
			t.Fatalf("run = %v, want an obs setup error", err)
		}
	})
}

var testPool = netip.MustParsePrefix("100.64.0.0/24")

// setObsEnv clears every variable that would export or disable telemetry and
// sets the metrics exporter and health address.
func setObsEnv(t *testing.T, exporter, health string) {
	t.Helper()
	for _, k := range []string{
		obs.EnvEndpoint, obs.EnvSDKDisabled, obs.EnvSentryDSN, obs.EnvTracesExporter, obs.EnvLogsExporter,
	} {
		t.Setenv(k, "")
	}
	t.Setenv(obs.EnvMetricsExporter, exporter)
	t.Setenv(obs.EnvHealthAddr, health)
}

// startRun starts run on a fresh SQLite file, waits until its gRPC health
// check answers, and returns a function that stops it and fails the test
// unless it returned nil.
func startRun(t *testing.T, listen string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	db := filepath.Join(t.TempDir(), "net.db")
	go func() {
		done <- run(ctx, listen, db, network.StoreConfig{}, testPool, network.DefaultWorkers, network.KubeOVNConfig{})
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
				t.Errorf("run: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("run did not stop")
		}
	}
	t.Cleanup(stop)
	conn, err := grpc.NewClient(listen, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() }) //nolint:errcheck // test client
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
		if err == nil {
			return stop
		}
		if time.Now().After(deadline) {
			t.Fatalf("gRPC health: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// poll gets url until ok accepts the answer and returns the last body.
func poll(t *testing.T, url string, ok func(int, string) bool) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, body, err := tryGet(t.Context(), url)
		if err == nil && ok(code, body) {
			return body
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: code %d err %v body %q", url, code, err, body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	code, body, err := tryGet(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	return code, body
}

func tryGet(ctx context.Context, url string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err //nolint:wrapcheck // test helper
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err //nolint:wrapcheck // test helper
	}
	defer resp.Body.Close() //nolint:errcheck // test read
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err //nolint:wrapcheck // test helper
}

// freeAddr returns a loopback address that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}
