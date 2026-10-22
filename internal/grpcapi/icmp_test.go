package grpcapi

import (
	"context"
	"net/netip"
	"path/filepath"
	"testing"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	networkv1 "github.com/popododo0720/vws-proto/gen/go/vws/network/v1"

	"github.com/popododo0720/vws-network/internal/network"
)

func TestSecurityGroupICMPTypeCodeOverGRPC(t *testing.T) {
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
	groups := networkv1.NewSecurityGroupServiceClient(conn)
	ops := longrunningpb.NewOperationsClient(conn)
	parent := "projects/proj/regions/r1"
	op, err := vpcs.CreateVpc(ctx, &networkv1.CreateVpcRequest{
		Parent: parent, VpcId: "net",
		Vpc: &networkv1.Vpc{Spec: &networkv1.VpcSpec{Ipv4Cidr: "10.90.0.0/16"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, ctx, ops, op.GetName())
	rule := func(id string, from, to int32) *networkv1.SecurityGroupRule {
		return &networkv1.SecurityGroupRule{
			Id: id, Direction: networkv1.Direction_DIRECTION_INGRESS,
			Protocol: networkv1.Protocol_PROTOCOL_ICMP, FromPort: from, ToPort: to,
			Target: &networkv1.SecurityGroupRule_Cidr{Cidr: "0.0.0.0/0"},
		}
	}
	op, err = groups.CreateSecurityGroup(ctx, &networkv1.CreateSecurityGroupRequest{
		Parent: parent + "/vpcs/net", SecurityGroupId: "icmp",
		SecurityGroup: &networkv1.SecurityGroup{Spec: &networkv1.SecurityGroupSpec{Rules: []*networkv1.SecurityGroupRule{
			rule("echo-reply", 0, 0),
			rule("all", -1, -1),
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, ctx, ops, op.GetName())
	got, err := groups.GetSecurityGroup(ctx, &networkv1.GetSecurityGroupRequest{
		Name: parent + "/vpcs/net/securityGroups/icmp",
	})
	if err != nil {
		t.Fatal(err)
	}
	rules := got.GetSpec().GetRules()
	if len(rules) != 2 {
		t.Fatalf("rules %+v", rules)
	}
	seen := map[[2]int32]bool{}
	for _, r := range rules {
		seen[[2]int32{r.GetFromPort(), r.GetToPort()}] = true
	}
	for _, want := range [][2]int32{{0, 0}, {-1, -1}} {
		if !seen[want] {
			t.Fatalf("missing icmp type %d code %d in %+v", want[0], want[1], rules)
		}
	}

	_, err = groups.CreateSecurityGroup(ctx, &networkv1.CreateSecurityGroupRequest{
		Parent: parent + "/vpcs/net", SecurityGroupId: "bad",
		SecurityGroup: &networkv1.SecurityGroup{Spec: &networkv1.SecurityGroupSpec{Rules: []*networkv1.SecurityGroupRule{
			rule("bad-icmp", -1, 0),
		}}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid icmp code = %s err %v", status.Code(err), err)
	}
}
