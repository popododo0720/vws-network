package grpcapi

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	commonv1 "github.com/popododo0720/vws-proto/gen/go/vws/common/v1"
	networkv1 "github.com/popododo0720/vws-proto/gen/go/vws/network/v1"

	"github.com/popododo0720/vws-network/internal/domain"
	"github.com/popododo0720/vws-network/internal/network"
)

func TestAllErrKindsMapped(t *testing.T) {
	for k := domain.Invalid; k <= domain.Busy; k++ {
		code := status.Code(toStatus(&domain.Error{Kind: k, Msg: "x"}, ""))
		if code == codes.OK || code == codes.Unknown || code == codes.Internal {
			t.Fatalf("kind %d mapped to %s", k, code)
		}
	}
}

func TestUnknownErrorHidesDetail(t *testing.T) {
	err := toStatus(errors.New("sqlite: secret schema"), "")
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Internal || st.Message() != "internal error" {
		t.Fatalf("status = %v", err)
	}
}

func TestOperationTypesAreRPCNames(t *testing.T) {
	names := map[string]bool{}
	for _, desc := range []*grpc.ServiceDesc{
		&networkv1.VpcService_ServiceDesc,
		&networkv1.SubnetService_ServiceDesc,
		&networkv1.SecurityGroupService_ServiceDesc,
		&networkv1.AddressService_ServiceDesc,
	} {
		for _, m := range desc.Methods {
			names[m.MethodName] = true
		}
	}
	want := []domain.OperationType{
		domain.OpCreateVPC, domain.OpUpdateVPC, domain.OpDeleteVPC,
		domain.OpCreateSubnet, domain.OpUpdateSubnet, domain.OpDeleteSubnet,
		domain.OpCreateSecurityGroup, domain.OpUpdateSecurityGroup, domain.OpDeleteSecurityGroup,
	}
	for _, typ := range want {
		if !names[string(typ)] {
			t.Fatalf("%s is not an rpc", typ)
		}
	}
}

func TestHandlersOverrideUnimplemented(t *testing.T) {
	hv := reflect.ValueOf(&handler{})
	descs := []*grpc.ServiceDesc{
		&networkv1.VpcService_ServiceDesc,
		&networkv1.SubnetService_ServiceDesc,
		&networkv1.SecurityGroupService_ServiceDesc,
		&networkv1.AddressService_ServiceDesc,
	}
	for _, desc := range descs {
		if len(desc.Methods) == 0 {
			t.Fatal("no methods")
		}
		for _, method := range desc.Methods {
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
	client := networkv1.NewVpcServiceClient(conn)
	ops := longrunningpb.NewOperationsClient(conn)
	parent := "projects/proj/regions/r1"
	key := "11111111-1111-4111-8111-111111111111"

	_, err = client.CreateVpc(ctx, &networkv1.CreateVpcRequest{
		Parent: parent, VpcId: "app", RequestId: key,
		Vpc: &networkv1.Vpc{Spec: &networkv1.VpcSpec{Ipv4Cidr: "10.0.0.1/16"}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad cidr code = %s err %v", status.Code(err), err)
	}

	op, err := client.CreateVpc(ctx, &networkv1.CreateVpcRequest{
		Parent: parent, VpcId: "app", RequestId: key,
		Vpc: &networkv1.Vpc{Spec: &networkv1.VpcSpec{Ipv4Cidr: "10.20.0.0/16"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := client.CreateVpc(ctx, &networkv1.CreateVpcRequest{
		Parent: parent, VpcId: "app", RequestId: key,
		Vpc: &networkv1.Vpc{Spec: &networkv1.VpcSpec{Ipv4Cidr: "10.20.0.0/16"}},
	})
	if err != nil || replay.GetName() != op.GetName() {
		t.Fatalf("replay %+v err %v", replay, err)
	}
	_, err = client.CreateVpc(ctx, &networkv1.CreateVpcRequest{
		Parent: parent, VpcId: "app", RequestId: key,
		Vpc: &networkv1.Vpc{Spec: &networkv1.VpcSpec{Ipv4Cidr: "10.21.0.0/16"}},
	})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("key reuse code = %s", status.Code(err))
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		op, err = ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.GetName()})
		if err != nil {
			t.Fatal(err)
		}
		if op.GetDone() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !op.GetDone() || op.GetError() != nil {
		t.Fatalf("operation %+v", op)
	}
	vpc, err := client.GetVpc(ctx, &networkv1.GetVpcRequest{Name: "projects/proj/regions/r1/vpcs/app"})
	if err != nil {
		t.Fatal(err)
	}
	if vpc.GetSpec().GetIpv4Cidr() != "10.20.0.0/16" || vpc.GetStatus().GetObserved().GetState() != commonv1.ReconciliationState_RECONCILIATION_STATE_READY {
		t.Fatalf("vpc %+v", vpc)
	}
	page, err := client.ListVpcs(ctx, &networkv1.ListVpcsRequest{Parent: parent})
	if err != nil || len(page.GetVpcs()) != 1 {
		t.Fatalf("list %+v err %v", page, err)
	}
}

func TestRequestIDAndUpdateMask(t *testing.T) {
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
	vpcs := networkv1.NewVpcServiceClient(conn)
	groups := networkv1.NewSecurityGroupServiceClient(conn)
	parent := "projects/proj/regions/r1"

	op, err := vpcs.CreateVpc(ctx, &networkv1.CreateVpcRequest{
		Parent: parent, VpcId: "app",
		Vpc: &networkv1.Vpc{Spec: &networkv1.VpcSpec{Ipv4Cidr: "10.40.0.0/16"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	del, err := vpcs.DeleteVpc(ctx, &networkv1.DeleteVpcRequest{Name: "projects/proj/regions/r1/vpcs/app"})
	if err != nil {
		t.Fatalf("delete without request id: %v", err)
	}
	if del.GetName() == op.GetName() {
		t.Fatal("delete reused the create operation")
	}

	op, err = vpcs.CreateVpc(ctx, &networkv1.CreateVpcRequest{
		Parent: parent, VpcId: "net", RequestId: "22222222-2222-4222-8222-222222222222",
		Vpc: &networkv1.Vpc{Spec: &networkv1.VpcSpec{Ipv4Cidr: "10.41.0.0/16"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, ctx, longrunningpb.NewOperationsClient(conn), op.GetName())
	_, err = groups.CreateSecurityGroup(ctx, &networkv1.CreateSecurityGroupRequest{
		Parent: "projects/proj/regions/r1/vpcs/net", SecurityGroupId: "web",
		RequestId: "33333333-3333-4333-8333-333333333333",
		SecurityGroup: &networkv1.SecurityGroup{Spec: &networkv1.SecurityGroupSpec{Rules: []*networkv1.SecurityGroupRule{{
			Id: "ssh", Direction: networkv1.Direction_DIRECTION_INGRESS, Protocol: networkv1.Protocol_PROTOCOL_TCP,
			FromPort: 22, ToPort: 22, Target: &networkv1.SecurityGroupRule_Cidr{Cidr: "0.0.0.0/0"},
		}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = groups.UpdateSecurityGroup(ctx, &networkv1.UpdateSecurityGroupRequest{
		SecurityGroup: &networkv1.SecurityGroup{
			Name: "projects/proj/regions/r1/vpcs/net/securityGroups/web",
			Spec: &networkv1.SecurityGroupSpec{},
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
		RequestId:  "44444444-4444-4444-8444-444444444444",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := groups.GetSecurityGroup(ctx, &networkv1.GetSecurityGroupRequest{
		Name: "projects/proj/regions/r1/vpcs/net/securityGroups/web",
	})
	if err != nil || len(got.GetSpec().GetRules()) != 1 || got.GetSpec().GetRules()[0].GetFromPort() != 22 {
		t.Fatalf("rules %+v err %v", got.GetSpec().GetRules(), err)
	}
}

func waitDone(t *testing.T, ctx context.Context, ops longrunningpb.OperationsClient, name string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		op, err := ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if op.GetDone() {
			if op.GetError() != nil {
				t.Fatalf("operation %s", op.GetError().GetMessage())
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("operation did not finish")
}
