package grpcapi

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/metric/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/popododo0720/vws-lib/obs"
)

// ServiceName is the service label in logs, metrics and traces.
const ServiceName = "network"

// mutating are the RPC name prefixes that change state and so are audited.
var mutating = []string{"Create", "Update", "Delete", "Allocate", "Release", "Associate", "Disassociate", "Authorize", "Revoke"}

// ServerOptions is the one place the server interceptors are chosen: tracing,
// vws.rpc.* metrics, audit, then panic recovery innermost so a panic is
// counted as Internal. vws-lib's grpcobs interceptors are not used because
// v0.1.0 rewrites a handler's gRPC status: it reports it as a server fault,
// repeats the message and drops the ErrorInfo details.
func ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.StatsHandler(otelgrpc.NewServerHandler(otelgrpc.WithMeterProvider(noop.NewMeterProvider()))),
		grpc.ChainUnaryInterceptor(rpcUnary, auditUnary, recoverUnary),
		grpc.ChainStreamInterceptor(rpcStream, recoverStream),
	}
}

func methodName(full string) string { return full[strings.LastIndex(full, "/")+1:] }

func rpcUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
	start := time.Now()
	resp, err := next(ctx, req)
	obs.M().RPCServer(ctx, ServiceName, info.FullMethod, status.Code(err).String(), time.Since(start))
	return resp, err
}

func rpcStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) error {
	start := time.Now()
	err := next(srv, ss)
	obs.M().RPCServer(ss.Context(), ServiceName, info.FullMethod, status.Code(err).String(), time.Since(start))
	return err
}

func auditUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
	resp, err := next(ctx, req)
	method := methodName(info.FullMethod)
	if !isMutating(method) {
		return resp, err
	}
	outcome := "ok"
	if err != nil {
		outcome = strings.ToLower(status.Code(err).String())
	}
	obs.M().Audit(ctx, ServiceName, method, outcome)
	slog.Default().LogAttrs(ctx, slog.LevelInfo, "audit",
		slog.Bool(obs.FieldAudit, true), slog.String(obs.FieldOp, method), slog.String("outcome", outcome))
	return resp, err
}

func isMutating(method string) bool {
	for _, p := range mutating {
		if strings.HasPrefix(method, p) {
			return true
		}
	}
	return false
}
