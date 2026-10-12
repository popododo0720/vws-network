package grpcapi

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/popododo0720/vws-lib/obs/report"
)

func recovered(ctx context.Context, r any) error {
	_ = report.Panic(ctx, r) //nolint:errcheck // the client gets the fixed status below, not the report's error
	return status.Error(codes.Internal, "internal error")
}

func recoverUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			resp, err = nil, recovered(ctx, r)
		}
	}()
	return next(ctx, req)
}

func recoverStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = recovered(streamContext(ss), r)
		}
	}()
	return next(srv, ss)
}

func streamContext(ss grpc.ServerStream) context.Context {
	if ss == nil {
		return context.Background()
	}
	return ss.Context()
}
