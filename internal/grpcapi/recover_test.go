package grpcapi

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRecoverUnaryReturnsInternalWithoutPanicValue(t *testing.T) {
	_, err := recoverUnary(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/vws.Test/Boom"},
		func(context.Context, any) (any, error) { panic("password=hunter2") })
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Internal || st.Message() != "internal error" {
		t.Fatalf("err %v, want Internal with a fixed message", err)
	}
}

func TestRecoverStreamReturnsInternalWithoutPanicValue(t *testing.T) {
	err := recoverStream(nil, nil, &grpc.StreamServerInfo{FullMethod: "/vws.Test/Boom"},
		func(any, grpc.ServerStream) error { panic("password=hunter2") })
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Internal || st.Message() != "internal error" {
		t.Fatalf("err %v, want Internal with a fixed message", err)
	}
}

func TestRecoverPassesResultsThrough(t *testing.T) {
	resp, err := recoverUnary(context.Background(), nil, &grpc.UnaryServerInfo{},
		func(context.Context, any) (any, error) { return "ok", nil })
	if err != nil || resp != "ok" {
		t.Fatalf("resp %v err %v", resp, err)
	}
}
