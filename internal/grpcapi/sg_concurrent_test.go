package grpcapi

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	networkv1 "github.com/popododo0720/vws-proto/gen/go/vws/network/v1"

	"github.com/popododo0720/vws-network/internal/network"
)

var errDuplicateRule = errors.New("InvalidPermission.Duplicate")

// authorizeRule is the read-modify-write a gateway such as console-api runs
// for AuthorizeSecurityGroupIngress: read the group, refuse a rule that is
// already there, write the union back with the etag it read, and start over
// when the write is aborted.
func authorizeRule(
	ctx context.Context, groups networkv1.SecurityGroupServiceClient, name string,
	rule *networkv1.SecurityGroupRule, ids *atomic.Uint64,
) error {
	for {
		got, err := groups.GetSecurityGroup(ctx, &networkv1.GetSecurityGroupRequest{Name: name})
		if err != nil {
			return err
		}
		for _, have := range got.GetSpec().GetRules() {
			if have.GetDirection() == rule.GetDirection() && have.GetProtocol() == rule.GetProtocol() &&
				have.GetFromPort() == rule.GetFromPort() && have.GetToPort() == rule.GetToPort() &&
				have.GetCidr() == rule.GetCidr() {
				return errDuplicateRule
			}
		}
		got.Spec.Rules = append(got.Spec.Rules, rule)
		_, err = groups.UpdateSecurityGroup(ctx, &networkv1.UpdateSecurityGroupRequest{
			SecurityGroup: got,
			UpdateMask:    &fieldmaskpb.FieldMask{Paths: []string{"spec.rules"}},
			RequestId:     fmt.Sprintf("00000000-0000-4000-8000-%012x", ids.Add(1)),
		})
		if status.Code(err) == codes.Aborted {
			continue
		}
		return err
	}
}

func TestConcurrentAuthorizeKeepsEveryRule(t *testing.T) {
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
	rule := func(port int32) *networkv1.SecurityGroupRule {
		return &networkv1.SecurityGroupRule{
			Id: fmt.Sprintf("tcp-%d", port), Direction: networkv1.Direction_DIRECTION_INGRESS,
			Protocol: networkv1.Protocol_PROTOCOL_TCP, FromPort: port, ToPort: port,
			Target: &networkv1.SecurityGroupRule_Cidr{Cidr: "0.0.0.0/0"},
		}
	}
	op, err = groups.CreateSecurityGroup(ctx, &networkv1.CreateSecurityGroupRequest{
		Parent: parent + "/vpcs/net", SecurityGroupId: "web",
		SecurityGroup: &networkv1.SecurityGroup{Spec: &networkv1.SecurityGroupSpec{
			Rules: []*networkv1.SecurityGroupRule{rule(22)},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, ctx, ops, op.GetName())
	name := parent + "/vpcs/net/securityGroups/web"

	const writers, dupWriters = 24, 4
	var ids atomic.Uint64
	var wg sync.WaitGroup
	errs := make([]error, writers+dupWriters)
	for i := range writers + dupWriters {
		port := int32(1000 + i)
		if i >= writers {
			port = 443
		}
		wg.Go(func() { errs[i] = authorizeRule(ctx, groups, name, rule(port), &ids) })
	}
	wg.Wait()

	added := 0
	for i, err := range errs {
		switch {
		case i < writers && err != nil:
			t.Errorf("writer %d: %v", i, err)
		case i >= writers && err == nil:
			added++
		case i >= writers && !errors.Is(err, errDuplicateRule):
			t.Errorf("duplicate writer %d: %v", i, err)
		}
	}
	if added != 1 {
		t.Errorf("%d of %d writers of the same rule succeeded, want 1 and the rest InvalidPermission.Duplicate",
			added, dupWriters)
	}
	got, err := groups.GetSecurityGroup(ctx, &networkv1.GetSecurityGroupRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	have := map[int32]bool{}
	for _, r := range got.GetSpec().GetRules() {
		have[r.GetFromPort()] = true
	}
	var lost []int32
	for _, port := range append([]int32{22, 443}, func() []int32 {
		var ps []int32
		for i := range writers {
			ps = append(ps, int32(1000+i))
		}
		return ps
	}()...) {
		if !have[port] {
			lost = append(lost, port)
		}
	}
	if len(lost) > 0 {
		t.Errorf("lost %d of %d authorized rules: ports %v", len(lost), writers+2, lost)
	}
}

func TestUpdateSecurityGroupRefusesStaleEtag(t *testing.T) {
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
	op, err = groups.CreateSecurityGroup(ctx, &networkv1.CreateSecurityGroupRequest{
		Parent: parent + "/vpcs/net", SecurityGroupId: "web",
		SecurityGroup: &networkv1.SecurityGroup{Spec: &networkv1.SecurityGroupSpec{Description: "v1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, ctx, ops, op.GetName())
	name := parent + "/vpcs/net/securityGroups/web"
	first, err := groups.GetSecurityGroup(ctx, &networkv1.GetSecurityGroupRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if first.GetEtag() == "" {
		t.Fatal("GetSecurityGroup returned no etag")
	}
	mask := &fieldmaskpb.FieldMask{Paths: []string{"spec.description"}}
	const key = "00000000-0000-4000-8000-000000000001"
	first.Spec.Description = "v2"
	update := func(key string) (*longrunningpb.Operation, error) {
		return groups.UpdateSecurityGroup(ctx, &networkv1.UpdateSecurityGroupRequest{
			SecurityGroup: first, UpdateMask: mask, RequestId: key,
		})
	}
	done, err := update(key)
	if err != nil {
		t.Fatalf("update with the current etag: %v", err)
	}
	replay, err := update(key)
	if err != nil || replay.GetName() != done.GetName() {
		t.Fatalf("replay after the etag moved = %v %v, want the stored operation %s", replay.GetName(), err, done.GetName())
	}
	first.Spec.Description = "v3"
	_, err = update(key)
	if status.Code(err) != codes.Aborted || reason(err) != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("reused request_id with a different body = %v, want Aborted IDEMPOTENCY_CONFLICT", err)
	}
	_, err = update("")
	if status.Code(err) != codes.Aborted || reason(err) != "ETAG_MISMATCH" {
		t.Fatalf("update with a stale etag = %v, want Aborted ETAG_MISMATCH", err)
	}
	got, err := groups.GetSecurityGroup(ctx, &networkv1.GetSecurityGroupRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetSpec().GetDescription() != "v2" || got.GetEtag() == first.GetEtag() {
		t.Fatalf("after stale write: description %q etag %q (read %q)",
			got.GetSpec().GetDescription(), got.GetEtag(), first.GetEtag())
	}
}

func reason(err error) string {
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.GetReason()
		}
	}
	return ""
}
