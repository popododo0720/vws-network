package grpcapi

import (
	"context"
	"log"
	"sort"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

const readyBudget = 2 * time.Second

// Health serves grpc.health.v1 for "" and every service registered on the
// server before RegisterHealth. All names share one status, computed by ready.
type Health struct {
	srv   *health.Server
	names []string
	ready func(context.Context) error
}

// RegisterHealth adds grpc.health.v1 to s. Call it after Register so every
// API service gets a health name. Every name starts NOT_SERVING.
func RegisterHealth(s *grpc.Server, ready func(context.Context) error) *Health {
	h := &Health{srv: health.NewServer(), names: []string{""}, ready: ready}
	for name := range s.GetServiceInfo() {
		h.names = append(h.names, name)
	}
	sort.Strings(h.names)
	h.set(healthpb.HealthCheckResponse_NOT_SERVING)
	healthpb.RegisterHealthServer(s, h.srv)
	return h
}

// Run checks readiness now and then every interval until ctx is done.
func (h *Health) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		h.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (h *Health) check(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, readyBudget)
	defer cancel()
	if err := h.ready(ctx); err != nil {
		log.Printf("health: not serving: %v", err)
		h.set(healthpb.HealthCheckResponse_NOT_SERVING)
		return
	}
	h.set(healthpb.HealthCheckResponse_SERVING)
}

func (h *Health) set(st healthpb.HealthCheckResponse_ServingStatus) {
	for _, n := range h.names {
		h.srv.SetServingStatus(n, st)
	}
}

// Shutdown sets every name NOT_SERVING for good. Call it before GracefulStop
// so endpoints drain.
func (h *Health) Shutdown() { h.srv.Shutdown() }
