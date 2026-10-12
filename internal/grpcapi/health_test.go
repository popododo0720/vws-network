package grpcapi

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

var apiServices = []string{
	"",
	"google.longrunning.Operations",
	"vws.network.v1.AddressService",
	"vws.network.v1.SecurityGroupService",
	"vws.network.v1.SubnetService",
	"vws.network.v1.VpcService",
}

func serveHealth(t *testing.T, ready func(context.Context) error) (*Health, healthpb.HealthClient) {
	t.Helper()
	lis, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	Register(gs, nil)
	h := RegisterHealth(gs, ready)
	go func() {
		if err := gs.Serve(lis); err != nil {
			t.Error(err)
		}
	}()
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	})
	return h, healthpb.NewHealthClient(conn)
}

func statusOf(t *testing.T, c healthpb.HealthClient, name string) healthpb.HealthCheckResponse_ServingStatus {
	t.Helper()
	r, err := c.Check(t.Context(), &healthpb.HealthCheckRequest{Service: name})
	if err != nil {
		t.Fatalf("check %q: %v", name, err)
	}
	return r.GetStatus()
}

func TestHealthNamesEveryRegisteredService(t *testing.T) {
	h, c := serveHealth(t, func(context.Context) error { return nil })
	if len(h.names) != len(apiServices) {
		t.Fatalf("health names %v, want %v", h.names, apiServices)
	}
	for i, n := range apiServices {
		if h.names[i] != n {
			t.Fatalf("health names %v, want %v", h.names, apiServices)
		}
		if got := statusOf(t, c, n); got != healthpb.HealthCheckResponse_NOT_SERVING {
			t.Fatalf("%q before first check = %s", n, got)
		}
	}
	h.check(t.Context())
	for _, n := range apiServices {
		if got := statusOf(t, c, n); got != healthpb.HealthCheckResponse_SERVING {
			t.Fatalf("%q = %s, want SERVING", n, got)
		}
	}
}

func TestHealthWatchSeesFlipAndShutdown(t *testing.T) {
	var fail atomic.Bool
	h, c := serveHealth(t, func(context.Context) error {
		if fail.Load() {
			return errors.New("store down")
		}
		return nil
	})
	h.check(t.Context())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	w, err := c.Watch(ctx, &healthpb.HealthCheckRequest{Service: ""})
	if err != nil {
		t.Fatal(err)
	}
	want := func(st healthpb.HealthCheckResponse_ServingStatus) {
		t.Helper()
		r, err := w.Recv()
		if err != nil || r.GetStatus() != st {
			t.Fatalf("watch = %v %v, want %s", r, err, st)
		}
	}
	want(healthpb.HealthCheckResponse_SERVING)
	fail.Store(true)
	h.check(t.Context())
	want(healthpb.HealthCheckResponse_NOT_SERVING)
	fail.Store(false)
	h.check(t.Context())
	want(healthpb.HealthCheckResponse_SERVING)
	h.Shutdown()
	want(healthpb.HealthCheckResponse_NOT_SERVING)
	h.check(t.Context())
	if got := statusOf(t, c, ""); got != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("after shutdown = %s", got)
	}
}
