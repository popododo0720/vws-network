package grpcapi

import (
	"context"
	"log"
	"runtime/debug"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ServerOptions returns the options every vws-network gRPC server needs. A
// panic in a handler is logged with its stack and reaches the client as
// Internal with a fixed message.
func ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(recoverUnary),
		grpc.ChainStreamInterceptor(recoverStream),
	}
}

func recovered(method string, r any) error {
	log.Printf("grpcapi: panic in %s: %v\n%s", method, r, debug.Stack())
	return status.Error(codes.Internal, "internal error")
}

func recoverUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			resp, err = nil, recovered(info.FullMethod, r)
		}
	}()
	return next(ctx, req)
}

func recoverStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = recovered(info.FullMethod, r)
		}
	}()
	return next(srv, ss)
}
