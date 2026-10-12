package grpcapi

import (
	"context"
	"net"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	networkv1 "github.com/popododo0720/vws-proto/gen/go/vws/network/v1"

	"github.com/popododo0720/vws-network/internal/domain"
)

const conflictMsg = `Vpc vpc-1 has id annotation "", want "vpc-1"`

type conflictVPCs struct {
	networkv1.UnimplementedVpcServiceServer
}

func (conflictVPCs) GetVpc(context.Context, *networkv1.GetVpcRequest) (*networkv1.Vpc, error) {
	return nil, toStatus(&domain.Error{Kind: domain.Conflict, Msg: conflictMsg}, "")
}

func TestServerOptionsKeepConflictStatus(t *testing.T) {
	lis, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(ServerOptions()...)
	networkv1.RegisterVpcServiceServer(gs, conflictVPCs{})
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
	_, err = networkv1.NewVpcServiceClient(conn).GetVpc(t.Context(), &networkv1.GetVpcRequest{})
	st := status.Convert(err)
	if st.Code() != codes.AlreadyExists || st.Message() != conflictMsg {
		t.Fatalf("status %v, want AlreadyExists %q", st, conflictMsg)
	}
	_, reason := statusInfo(&domain.Error{Kind: domain.Conflict}, "")
	var got []string
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			got = append(got, info.GetReason())
		}
	}
	if reason != "" && (len(got) != 1 || got[0] != reason) {
		t.Fatalf("ErrorInfo reasons %v, want [%s]", got, reason)
	}
}

func TestAuditedMethods(t *testing.T) {
	for m, want := range map[string]bool{"CreateVpc": true, "DeleteSubnet": true, "AuthorizeSecurityGroupIngress": true, "GetVpc": false, "ListOperations": false} {
		if isMutating(m) != want {
			t.Errorf("isMutating(%s) = %v, want %v", m, !want, want)
		}
	}
}
