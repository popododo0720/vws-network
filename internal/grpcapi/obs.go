package grpcapi

import (
	"context"
	"log/slog"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/popododo0720/vws-lib/obs"
	"github.com/popododo0720/vws-lib/obs/grpcobs"
)

// ServiceName is the service label in logs, metrics and traces.
const ServiceName = "network"

// mutating are the RPC name prefixes that change state and so are audited.
var mutating = []string{"Create", "Update", "Delete", "Allocate", "Release", "Associate", "Disassociate", "Authorize", "Revoke"}

// ServerOptions is the one place the server interceptors are chosen: vws-lib
// grpcobs (tracing, vws.rpc.* metrics, panic recovery, one error report, and
// errs status mapping that keeps a handler's gRPC status), then audit.
func ServerOptions() []grpc.ServerOption {
	return append(grpcobs.ServerOptions(ServiceName), grpc.ChainUnaryInterceptor(auditUnary))
}

func methodName(full string) string { return full[strings.LastIndex(full, "/")+1:] }

// auditUnary records a mutating RPC. A panic is audited as internal and then
// re-raised for grpcobs to recover.
func auditUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (resp any, err error) {
	method := methodName(info.FullMethod)
	if !isMutating(method) {
		return next(ctx, req)
	}
	panicked := true
	defer func() {
		outcome := "ok"
		switch {
		case panicked:
			outcome = strings.ToLower(codes.Internal.String())
		case err != nil:
			outcome = strings.ToLower(status.Code(err).String())
		}
		audit(ctx, method, outcome)
	}()
	resp, err = next(ctx, req)
	panicked = false
	return resp, err
}

func audit(ctx context.Context, method, outcome string) {
	obs.M().Audit(ctx, ServiceName, method, outcome)
	slog.Default().LogAttrs(ctx, slog.LevelInfo, "audit",
		slog.Bool(obs.FieldAudit, true), slog.String(obs.FieldOp, method), slog.String("outcome", outcome))
}

func isMutating(method string) bool {
	for _, p := range mutating {
		if strings.HasPrefix(method, p) {
			return true
		}
	}
	return false
}
