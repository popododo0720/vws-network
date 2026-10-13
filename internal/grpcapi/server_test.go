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
	for k := domain.Invalid; k <= domain.Forbidden; k++ {
		code := status.Code(toStatus(&domain.Error{Kind: k, Msg: "x"}, ""))
		if code == codes.OK || code == codes.Unknown || code == codes.Internal {
			t.Fatalf("kind %d mapped to %s", k, code)
		}
	}
}

func TestForbiddenIsPermissionDenied(t *testing.T) {
	if code := status.Code(toStatus(&domain.Error{Kind: domain.Forbidden, Msg: "x"}, "")); code != codes.PermissionDenied {
		t.Fatalf("Forbidden mapped to %s", code)
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
			var err error
			if e, ok := out[1].Interface().(error); ok {
				err = e
			}
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
	defer closeOrFail(t, svc.Close)

	lis, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	Register(gs, svc)
	go func() {
		if err := gs.Serve(lis); err != nil {
			t.Error(err)
		}
	}()
	defer gs.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, conn.Close)
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
	defer closeOrFail(t, svc.Close)
	lis, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	Register(gs, svc)
	go func() {
		if err := gs.Serve(lis); err != nil {
			t.Error(err)
		}
	}()
	defer gs.Stop()
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, conn.Close)
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

func TestAccountProjectStoresLabels(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "net.db")
	svc, err := network.Open(ctx, network.Config{
		DatabasePath: path,
		AddressPool:  netip.MustParsePrefix("198.51.100.0/24"),
	})
	if err != nil {
		t.Fatal(err)
	}
	conn := serve(t, svc)
	vpcs := networkv1.NewVpcServiceClient(conn)
	subs := networkv1.NewSubnetServiceClient(conn)
	groups := networkv1.NewSecurityGroupServiceClient(conn)
	addrs := networkv1.NewAddressServiceClient(conn)
	ops := longrunningpb.NewOperationsClient(conn)
	parent := "projects/a1001/regions/r1"
	key := "55555555-5555-4555-8555-555555555555"
	vpc := &networkv1.Vpc{
		Labels: map[string]string{"vws-fingerprint": "fp-1"},
		Spec:   &networkv1.VpcSpec{Ipv4Cidr: "10.70.0.0/16", Description: "edge"},
	}

	_, err = vpcs.CreateVpc(ctx, &networkv1.CreateVpcRequest{
		Parent: parent, VpcId: "bad", RequestId: key,
		Vpc: &networkv1.Vpc{Labels: map[string]string{"Bad": "x"}, Spec: &networkv1.VpcSpec{Ipv4Cidr: "10.71.0.0/16"}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad label code = %s err %v", status.Code(err), err)
	}
	_, err = vpcs.UpdateVpc(ctx, &networkv1.UpdateVpcRequest{
		Vpc:        &networkv1.Vpc{Name: parent + "/vpcs/app"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels.foo"}},
		RequestId:  "88888888-8888-4888-8888-888888888888",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("labels.foo code = %s err %v", status.Code(err), err)
	}
	_, err = vpcs.UpdateVpc(ctx, &networkv1.UpdateVpcRequest{
		Vpc: &networkv1.Vpc{
			Name:   parent + "/vpcs/missing",
			Labels: map[string]string{"vws-fingerprint": "fp"},
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
		RequestId:  "99999999-9999-4999-8999-999999999999",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("missing vpc code = %s err %v", status.Code(err), err)
	}
	op, err := vpcs.CreateVpc(ctx, &networkv1.CreateVpcRequest{
		Parent: parent, VpcId: "app", RequestId: key, Vpc: vpc,
	})
	if err != nil {
		t.Fatalf("rejected label consumed the request id: %v", err)
	}
	replay, err := vpcs.CreateVpc(ctx, &networkv1.CreateVpcRequest{
		Parent: parent, VpcId: "app", RequestId: key, Vpc: vpc,
	})
	if err != nil || replay.GetName() != op.GetName() {
		t.Fatalf("replay %+v err %v", replay, err)
	}
	_, err = vpcs.CreateVpc(ctx, &networkv1.CreateVpcRequest{
		Parent: parent, VpcId: "app", RequestId: key,
		Vpc: &networkv1.Vpc{Spec: &networkv1.VpcSpec{Ipv4Cidr: "10.72.0.0/16"}},
	})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("request id reuse code = %s", status.Code(err))
	}
	waitDone(t, ctx, ops, op.GetName())

	got, err := vpcs.GetVpc(ctx, &networkv1.GetVpcRequest{Name: parent + "/vpcs/app"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetSpec().GetDescription() != "edge" || got.GetLabels()["vws-fingerprint"] != "fp-1" {
		t.Fatalf("vpc %+v", got)
	}
	page, err := vpcs.ListVpcs(ctx, &networkv1.ListVpcsRequest{Parent: parent})
	if err != nil || len(page.GetVpcs()) != 1 || page.GetVpcs()[0].GetLabels()["vws-fingerprint"] != "fp-1" {
		t.Fatalf("list %+v err %v", page, err)
	}

	upKey := "66666666-6666-4666-8666-666666666666"
	up, err := vpcs.UpdateVpc(ctx, &networkv1.UpdateVpcRequest{
		Vpc: &networkv1.Vpc{
			Name: parent + "/vpcs/app",
			Spec: &networkv1.VpcSpec{Description: "edge-2"},
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.description"}},
		RequestId:  upKey,
	})
	if err != nil || !up.GetDone() {
		t.Fatalf("description update %+v err %v", up, err)
	}
	upReplay, err := vpcs.UpdateVpc(ctx, &networkv1.UpdateVpcRequest{
		Vpc: &networkv1.Vpc{
			Name: parent + "/vpcs/app",
			Spec: &networkv1.VpcSpec{Description: "edge-2"},
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.description"}},
		RequestId:  upKey,
	})
	if err != nil || upReplay.GetName() != up.GetName() {
		t.Fatalf("update replay %+v err %v", upReplay, err)
	}
	_, err = vpcs.UpdateVpc(ctx, &networkv1.UpdateVpcRequest{
		Vpc:        &networkv1.Vpc{Name: parent + "/vpcs/app", Labels: map[string]string{"vws-fingerprint": "fp-2"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
		RequestId:  "77777777-7777-4777-8777-777777777777",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err = vpcs.GetVpc(ctx, &networkv1.GetVpcRequest{Name: parent + "/vpcs/app"})
	if err != nil || got.GetSpec().GetDescription() != "edge-2" || got.GetLabels()["vws-fingerprint"] != "fp-2" || got.GetSpec().GetIpv4Cidr() != "10.70.0.0/16" {
		t.Fatalf("updated vpc %+v err %v", got, err)
	}

	if _, err := subs.CreateSubnet(ctx, &networkv1.CreateSubnetRequest{
		Parent: parent + "/vpcs/app", SubnetId: "web",
		Subnet: &networkv1.Subnet{
			Labels: map[string]string{"vws-fingerprint": "sub"},
			Spec:   &networkv1.SubnetSpec{Zone: "az1", Ipv4Cidr: "10.70.1.0/24", Description: "web"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	sub, err := subs.GetSubnet(ctx, &networkv1.GetSubnetRequest{Name: parent + "/vpcs/app/subnets/web"})
	if err != nil || sub.GetSpec().GetDescription() != "web" || sub.GetLabels()["vws-fingerprint"] != "sub" {
		t.Fatalf("subnet %+v err %v", sub, err)
	}
	if _, err := groups.CreateSecurityGroup(ctx, &networkv1.CreateSecurityGroupRequest{
		Parent: parent + "/vpcs/app", SecurityGroupId: "web",
		SecurityGroup: &networkv1.SecurityGroup{
			Labels: map[string]string{"vws-fingerprint": "sg"},
			Spec:   &networkv1.SecurityGroupSpec{Description: "group"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := groups.UpdateSecurityGroup(ctx, &networkv1.UpdateSecurityGroupRequest{
		SecurityGroup: &networkv1.SecurityGroup{
			Name:   parent + "/vpcs/app/securityGroups/web",
			Labels: map[string]string{"vws-fingerprint": "sg-2"},
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
	}); err != nil {
		t.Fatal(err)
	}
	group, err := groups.GetSecurityGroup(ctx, &networkv1.GetSecurityGroupRequest{Name: parent + "/vpcs/app/securityGroups/web"})
	if err != nil || group.GetSpec().GetDescription() != "group" || group.GetLabels()["vws-fingerprint"] != "sg-2" {
		t.Fatalf("group %+v err %v", group, err)
	}
	if _, err := svc.AllocateElasticIP(ctx, domain.Call{Project: "a1001", Key: "eip"}, domain.AllocateElasticIP{VPC: "app", ID: "eip", Region: "r1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := addrs.UpdateAddress(ctx, &networkv1.UpdateAddressRequest{
		Address: &networkv1.Address{
			Name:   parent + "/addresses/eip",
			Labels: map[string]string{"vws-fingerprint": "eip"},
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
	}); err != nil {
		t.Fatal(err)
	}
	addr, err := addrs.GetAddress(ctx, &networkv1.GetAddressRequest{Name: parent + "/addresses/eip"})
	if err != nil || addr.GetLabels()["vws-fingerprint"] != "eip" {
		t.Fatalf("address %+v err %v", addr, err)
	}

	closeOrFail(t, conn.Close)
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	svc, err = network.Open(ctx, network.Config{DatabasePath: path, AddressPool: netip.MustParsePrefix("198.51.100.0/24")})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, svc.Close)
	conn = serve(t, svc)
	defer closeOrFail(t, conn.Close)
	vpcs = networkv1.NewVpcServiceClient(conn)
	subs = networkv1.NewSubnetServiceClient(conn)
	groups = networkv1.NewSecurityGroupServiceClient(conn)
	addrs = networkv1.NewAddressServiceClient(conn)
	got, err = vpcs.GetVpc(ctx, &networkv1.GetVpcRequest{Name: parent + "/vpcs/app"})
	if err != nil || got.GetSpec().GetDescription() != "edge-2" || got.GetLabels()["vws-fingerprint"] != "fp-2" {
		t.Fatalf("reopened vpc %+v err %v", got, err)
	}
	sub, err = subs.GetSubnet(ctx, &networkv1.GetSubnetRequest{Name: parent + "/vpcs/app/subnets/web"})
	if err != nil || sub.GetSpec().GetDescription() != "web" || sub.GetLabels()["vws-fingerprint"] != "sub" {
		t.Fatalf("reopened subnet %+v err %v", sub, err)
	}
	group, err = groups.GetSecurityGroup(ctx, &networkv1.GetSecurityGroupRequest{Name: parent + "/vpcs/app/securityGroups/web"})
	if err != nil || group.GetSpec().GetDescription() != "group" || group.GetLabels()["vws-fingerprint"] != "sg-2" {
		t.Fatalf("reopened group %+v err %v", group, err)
	}
	addr, err = addrs.GetAddress(ctx, &networkv1.GetAddressRequest{Name: parent + "/addresses/eip"})
	if err != nil || addr.GetLabels()["vws-fingerprint"] != "eip" {
		t.Fatalf("reopened address %+v err %v", addr, err)
	}
}

func serve(t *testing.T, svc *network.Service) *grpc.ClientConn {
	t.Helper()
	lis, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	Register(gs, svc)
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
	return conn
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

func TestAddressVPCAndAssociation(t *testing.T) {
	ctx := context.Background()
	svc, err := network.Open(ctx, network.Config{
		DatabasePath: filepath.Join(t.TempDir(), "net.db"),
		AddressPool:  netip.MustParsePrefix("198.51.100.0/24"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, svc.Close)
	conn := serve(t, svc)
	defer closeOrFail(t, conn.Close)
	vpcs := networkv1.NewVpcServiceClient(conn)
	addrs := networkv1.NewAddressServiceClient(conn)
	parent := "projects/a1001/regions/r1"
	vpcName := parent + "/vpcs/app"
	const (
		instance = "projects/a1001/zones/z1/instances/i1"
		other    = "projects/a1001/zones/z1/instances/i2"
	)
	if _, err := vpcs.CreateVpc(ctx, &networkv1.CreateVpcRequest{
		Parent: parent, VpcId: "app",
		Vpc: &networkv1.Vpc{Spec: &networkv1.VpcSpec{Ipv4Cidr: "10.70.0.0/16"}},
	}); err != nil {
		t.Fatal(err)
	}
	create := func(id string, spec *networkv1.AddressSpec) error {
		spec.IpVersion = networkv1.IpVersion_IP_VERSION_IPV4
		_, err := addrs.CreateAddress(ctx, &networkv1.CreateAddressRequest{
			Parent: parent, AddressId: id, Address: &networkv1.Address{Spec: spec},
		})
		return err
	}
	update := func(name string, spec *networkv1.AddressSpec, paths ...string) error {
		spec.IpVersion = networkv1.IpVersion_IP_VERSION_IPV4
		_, err := addrs.UpdateAddress(ctx, &networkv1.UpdateAddressRequest{
			Address:    &networkv1.Address{Name: name, Spec: spec},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
		})
		return err
	}
	get := func(id string) *networkv1.Address {
		t.Helper()
		a, err := addrs.GetAddress(ctx, &networkv1.GetAddressRequest{Name: parent + "/addresses/" + id})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	if err := create("none", &networkv1.AddressSpec{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("create without vpc = %v", err)
	}
	if err := create("far", &networkv1.AddressSpec{Vpc: "projects/other/regions/r1/vpcs/app"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("create in another project's vpc = %v", err)
	}
	if err := create("far", &networkv1.AddressSpec{Vpc: "projects/a1001/regions/r2/vpcs/app"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("create in another region's vpc = %v", err)
	}
	if err := create("missing", &networkv1.AddressSpec{Vpc: parent + "/vpcs/nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("create in a missing vpc = %v", err)
	}
	if err := create("half", &networkv1.AddressSpec{
		Vpc: vpcName, Association: &networkv1.AddressAssociation{Instance: instance},
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("create with a half association = %v", err)
	}
	if err := create("eip", &networkv1.AddressSpec{Vpc: vpcName}); err != nil {
		t.Fatal(err)
	}
	if got := get("eip").GetSpec().GetVpc(); got != vpcName {
		t.Fatalf("stored vpc = %q", got)
	}

	assoc := &networkv1.AddressAssociation{Instance: instance, DeviceName: "nic0"}
	if err := update(parent+"/addresses/eip", &networkv1.AddressSpec{Association: assoc}, "spec.association"); err != nil {
		t.Fatal(err)
	}
	a := get("eip")
	if a.GetSpec().GetAssociation().GetInstance() != instance || a.GetStatus().GetDeviceName() != "nic0" || a.GetSpec().GetVpc() != vpcName {
		t.Fatalf("attached address = %+v", a)
	}
	if err := create("second", &networkv1.AddressSpec{Vpc: vpcName, Association: assoc}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("second address on one interface = %v", err)
	}
	// An empty vpc under mask spec keeps the stored vpc; the mask spec replaces the attachment.
	if err := update(parent+"/addresses/eip", &networkv1.AddressSpec{
		Association: &networkv1.AddressAssociation{Instance: other, DeviceName: "nic0"},
	}, "spec"); err != nil {
		t.Fatal(err)
	}
	a = get("eip")
	if a.GetSpec().GetVpc() != vpcName || a.GetSpec().GetAssociation().GetInstance() != other {
		t.Fatalf("after spec update = %+v", a)
	}
	if err := update(parent+"/addresses/eip", &networkv1.AddressSpec{Vpc: parent + "/vpcs/app2"}, "spec"); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("vpc change = %v", err)
	}
	if got := get("eip").GetSpec().GetVpc(); got != vpcName {
		t.Fatalf("vpc after rejected change = %q", got)
	}
	if err := update(parent+"/addresses/eip", &networkv1.AddressSpec{}, "spec.association"); err != nil {
		t.Fatal(err)
	}
	a = get("eip")
	if a.GetSpec().GetAssociation() != nil || a.GetStatus().GetInstance() != "" || a.GetSpec().GetVpc() != vpcName {
		t.Fatalf("after clear = %+v", a)
	}
	// Labels only leave the attachment and vpc alone.
	if err := update(parent+"/addresses/eip", &networkv1.AddressSpec{Association: assoc}, "spec.association"); err != nil {
		t.Fatal(err)
	}
	if _, err := addrs.UpdateAddress(ctx, &networkv1.UpdateAddressRequest{
		Address:    &networkv1.Address{Name: parent + "/addresses/eip", Labels: map[string]string{"k": "v"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
	}); err != nil {
		t.Fatal(err)
	}
	if a = get("eip"); a.GetSpec().GetAssociation().GetInstance() != instance || a.GetLabels()["k"] != "v" {
		t.Fatalf("after label update = %+v", a)
	}
	if _, err := addrs.DeleteAddress(ctx, &networkv1.DeleteAddressRequest{Name: parent + "/addresses/eip"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete while attached = %v", err)
	}
	// allow_missing creates through the create rules and may set the vpc.
	if _, err := addrs.UpdateAddress(ctx, &networkv1.UpdateAddressRequest{
		Address: &networkv1.Address{
			Name: parent + "/addresses/made",
			Spec: &networkv1.AddressSpec{IpVersion: networkv1.IpVersion_IP_VERSION_IPV4, Vpc: vpcName},
		},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"spec"}},
		AllowMissing: true,
	}); err != nil {
		t.Fatal(err)
	}
	if got := get("made").GetSpec().GetVpc(); got != vpcName {
		t.Fatalf("created through update, vpc = %q", got)
	}
}

func closeOrFail(t testing.TB, f func() error) {
	t.Helper()
	if err := f(); err != nil {
		t.Error(err)
	}
}

func TestFailedDataplaneOperationError(t *testing.T) {
	op := domain.Operation{ID: "op1", State: domain.OpFailed, Err: &domain.Error{Kind: domain.Dataplane, Msg: "dataplane reconciliation failed"}}
	out, err := (&handler{}).toOp("proj", "", "projects/proj/vpcs/v", commonv1.OperationVerb_OPERATION_VERB_CREATE, op)
	if err != nil {
		t.Fatal(err)
	}
	got := out.GetError()
	if !out.Done || codes.Code(got.GetCode()) != codes.Unavailable || got.GetMessage() != "dataplane reconciliation failed" {
		t.Fatalf("error = %v", got)
	}
}
