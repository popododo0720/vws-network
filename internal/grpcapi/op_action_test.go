package grpcapi

import (
	"context"
	"net/netip"
	"path/filepath"
	"testing"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	commonv1 "github.com/popododo0720/vws-proto/gen/go/vws/common/v1"
	networkv1 "github.com/popododo0720/vws-proto/gen/go/vws/network/v1"

	"github.com/popododo0720/vws-network/internal/domain"
	"github.com/popododo0720/vws-network/internal/network"
)

// TestAddressOperationAction checks that an operation returned by
// UpdateAddress records whether the association was set or cleared, and that
// GetOperation still reports it after the operation finished and the address
// changed again.
func TestAddressOperationAction(t *testing.T) {
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
	ops := longrunningpb.NewOperationsClient(conn)
	parent := "projects/a1001/regions/r1"
	name := parent + "/addresses/eip"
	assoc := &networkv1.AddressAssociation{Instance: "projects/a1001/zones/z1/instances/i1", DeviceName: "nic0"}
	other := &networkv1.AddressAssociation{Instance: "projects/a1001/zones/z1/instances/i2", DeviceName: "nic0"}

	vpcOp, err := vpcs.CreateVpc(ctx, &networkv1.CreateVpcRequest{
		Parent: parent, VpcId: "app",
		Vpc: &networkv1.Vpc{Spec: &networkv1.VpcSpec{Ipv4Cidr: "10.70.0.0/16"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, ctx, ops, vpcOp.GetName())
	created, err := addrs.CreateAddress(ctx, &networkv1.CreateAddressRequest{
		Parent: parent, AddressId: "eip",
		Address: &networkv1.Address{Spec: &networkv1.AddressSpec{IpVersion: networkv1.IpVersion_IP_VERSION_IPV4, Vpc: parent + "/vpcs/app"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, ctx, ops, created.GetName())

	update := func(requestID string, a *networkv1.Address, paths ...string) (*longrunningpb.Operation, error) {
		a.Name = name
		return addrs.UpdateAddress(ctx, &networkv1.UpdateAddressRequest{
			Address: a, RequestId: requestID, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
		})
	}
	spec := func(a *networkv1.AddressAssociation) *networkv1.Address {
		return &networkv1.Address{Spec: &networkv1.AddressSpec{IpVersion: networkv1.IpVersion_IP_VERSION_IPV4, Association: a}}
	}
	meta := func(t *testing.T, op *longrunningpb.Operation) *commonv1.OperationMetadata {
		t.Helper()
		m := &commonv1.OperationMetadata{}
		if err := op.GetMetadata().UnmarshalTo(m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	step := func(t *testing.T, requestID string, a *networkv1.Address, want string, paths ...string) string {
		t.Helper()
		op, err := update(requestID, a, paths...)
		if err != nil {
			t.Fatal(err)
		}
		if m := meta(t, op); m.GetAction() != want || m.GetVerb() != commonv1.OperationVerb_OPERATION_VERB_UPDATE {
			t.Fatalf("accepted %s: action %q verb %s, want %q UPDATE", requestID, m.GetAction(), m.GetVerb(), want)
		}
		waitDone(t, ctx, ops, op.GetName())
		return op.GetName()
	}

	const (
		assocKey   = "11111111-1111-4111-8111-111111111111"
		clearKey   = "22222222-2222-4222-8222-222222222222"
		labelKey   = "33333333-3333-4333-8333-333333333333"
		replaceKey = "44444444-4444-4444-8444-444444444444"
		specKey    = "55555555-5555-4555-8555-555555555555"
	)
	associated := step(t, assocKey, spec(assoc), actionAssociateAddress, "spec.association")
	cleared := step(t, clearKey, spec(nil), actionDisassociateAddress, "spec.association")
	labeled := step(t, labelKey, &networkv1.Address{Labels: map[string]string{"k": "v"}}, "", "labels")
	replaced := step(t, replaceKey, spec(assoc), actionAssociateAddress, "spec.association")
	// Mask spec replaces the attachment too, so it is an association.
	viaSpec := step(t, specKey, spec(other), actionAssociateAddress, "spec")

	// After every operation finished and the address moved on, each one
	// still reports its own action.
	for _, c := range []struct{ name, want string }{
		{associated, actionAssociateAddress},
		{cleared, actionDisassociateAddress},
		{labeled, ""},
		{replaced, actionAssociateAddress},
		{viaSpec, actionAssociateAddress},
	} {
		op, err := ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: c.name})
		if err != nil {
			t.Fatal(err)
		}
		if !op.GetDone() || op.GetError() != nil {
			t.Fatalf("%s not done: %+v", c.name, op)
		}
		if got := meta(t, op).GetAction(); got != c.want {
			t.Fatalf("GetOperation(%s) action = %q, want %q", c.name, got, c.want)
		}
	}

	// Replaying a request id returns the same operation and action; reusing it
	// for the other action is a key conflict.
	replay, err := update(clearKey, spec(nil), "spec.association")
	if err != nil {
		t.Fatal(err)
	}
	if replay.GetName() != cleared || meta(t, replay).GetAction() != actionDisassociateAddress {
		t.Fatalf("replay = %s %q, want %s DisassociateAddress", replay.GetName(), meta(t, replay).GetAction(), cleared)
	}
	if _, err := update(clearKey, spec(assoc), "spec.association"); status.Code(err) != codes.Aborted {
		t.Fatalf("key reused for associate = %v, want Aborted", err)
	}

	// validate_only reports the action it would take without changing anything.
	dry, err := addrs.UpdateAddress(ctx, &networkv1.UpdateAddressRequest{
		Address: spec(nil), UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.association"}}, ValidateOnly: true,
	})
	if err == nil {
		t.Fatalf("validate_only without a name = %v", dry)
	}
	a := spec(nil)
	a.Name = name
	dry, err = addrs.UpdateAddress(ctx, &networkv1.UpdateAddressRequest{
		Address: a, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.association"}}, ValidateOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := meta(t, dry).GetAction(); got != actionDisassociateAddress {
		t.Fatalf("validate_only action = %q", got)
	}
	got, err := addrs.GetAddress(ctx, &networkv1.GetAddressRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetSpec().GetAssociation().GetInstance() != other.GetInstance() {
		t.Fatalf("validate_only changed the address: %+v", got)
	}
}

func TestActionOfNonAddressOperations(t *testing.T) {
	out, err := (&handler{}).toOp("proj", "", "projects/proj/regions/r1/vpcs/v", commonv1.OperationVerb_OPERATION_VERB_UPDATE, opOf("UpdateVpc"))
	if err != nil {
		t.Fatal(err)
	}
	m := &commonv1.OperationMetadata{}
	if err := out.GetMetadata().UnmarshalTo(m); err != nil {
		t.Fatal(err)
	}
	if m.GetAction() != "" {
		t.Fatalf("UpdateVpc action = %q, want empty", m.GetAction())
	}
}

func opOf(typ string) domain.Operation {
	return domain.Operation{ID: "op1", Type: domain.OperationType(typ), State: domain.OpSucceeded}
}
