package grpcapi

import (
	"context"
	"net"
	"net/netip"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	networkv1 "github.com/popododo0720/vws-network/gen/network/v1"
	"github.com/popododo0720/vws-network/internal/domain"
	"github.com/popododo0720/vws-network/internal/network"
)

func TestAllErrKindsMapped(t *testing.T) {
	for k := domain.Invalid; k <= domain.Dataplane; k++ {
		code := status.Code(toStatus(&domain.Error{Kind: k, Msg: "x"}))
		if code == codes.OK || code == codes.Unknown || code == codes.Internal {
			t.Fatalf("kind %d mapped to %s", k, code)
		}
	}
}

func TestOperationTypesAreRPCNames(t *testing.T) {
	names := map[string]bool{}
	for _, m := range networkv1.NetworkService_ServiceDesc.Methods {
		names[m.MethodName] = true
	}
	if len(domain.OperationTypes()) == 0 {
		t.Fatal("no operation types")
	}
	for _, typ := range domain.OperationTypes() {
		if !names[string(typ)] {
			t.Fatalf("%s is not an rpc", typ)
		}
	}
}

func TestHandlersOverrideUnimplemented(t *testing.T) {
	if len(networkv1.NetworkService_ServiceDesc.Methods) == 0 {
		t.Fatal("no methods")
	}
	// A nil request is rejected by every real handler before it touches the
	// service. The embedded stub answers Unimplemented instead.
	hv := reflect.ValueOf(&handler{})
	for _, method := range networkv1.NetworkService_ServiceDesc.Methods {
		m := hv.MethodByName(method.MethodName)
		if !m.IsValid() {
			t.Fatalf("handler missing %s", method.MethodName)
		}
		out := m.Call([]reflect.Value{
			reflect.ValueOf(context.Background()),
			reflect.Zero(m.Type().In(1)),
		})
		err, _ := out[1].Interface().(error)
		if status.Code(err) == codes.Unimplemented {
			t.Fatalf("%s is still the unimplemented stub", method.MethodName)
		}
	}
}

func TestCreateVPCOverGRPC(t *testing.T) {
	ctx := context.Background()
	svc, err := network.Open(ctx, network.Config{
		DatabasePath: filepath.Join(t.TempDir(), "net.db"),
		AddressPool:  netip.MustParsePrefix("198.51.100.0/24"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	Register(gs, svc)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := networkv1.NewNetworkServiceClient(conn)

	_, err = client.CreateVpc(ctx, &networkv1.CreateVpcRequest{
		ProjectId: "proj", IdempotencyKey: "bad", CidrBlock: "10.0.0.1/16",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad cidr code = %s err %v", status.Code(err), err)
	}

	op, err := client.CreateVpc(ctx, &networkv1.CreateVpcRequest{
		ProjectId: "proj", IdempotencyKey: "vpc", CidrBlock: "10.20.0.0/16",
	})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := client.CreateVpc(ctx, &networkv1.CreateVpcRequest{
		ProjectId: "proj", IdempotencyKey: "vpc", CidrBlock: "10.20.0.0/16",
	})
	if err != nil || replay.GetId() != op.GetId() {
		t.Fatalf("replay %+v err %v", replay, err)
	}
	_, err = client.CreateVpc(ctx, &networkv1.CreateVpcRequest{
		ProjectId: "proj", IdempotencyKey: "vpc", CidrBlock: "10.21.0.0/16",
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("key reuse code = %s", status.Code(err))
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		op, err = client.GetOperation(ctx, &networkv1.GetOperationRequest{ProjectId: "proj", OperationId: op.GetId()})
		if err != nil {
			t.Fatal(err)
		}
		if op.GetState() == networkv1.Operation_SUCCEEDED || op.GetState() == networkv1.Operation_FAILED {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if op.GetState() != networkv1.Operation_SUCCEEDED {
		t.Fatalf("operation %+v", op)
	}
	vpc, err := client.GetVpc(ctx, &networkv1.GetVpcRequest{ProjectId: "proj", VpcId: op.GetResourceId()})
	if err != nil {
		t.Fatal(err)
	}
	if vpc.GetCidrBlock() != "10.20.0.0/16" || vpc.GetStatus() != networkv1.ResourceStatus_RESOURCE_STATUS_AVAILABLE || vpc.GetDefaultSecurityGroupId() == "" {
		t.Fatalf("vpc %+v", vpc)
	}
	page, err := client.ListVpcs(ctx, &networkv1.ListVpcsRequest{ProjectId: "proj"})
	if err != nil || len(page.GetVpcs()) != 1 {
		t.Fatalf("list %+v err %v", page, err)
	}
}
