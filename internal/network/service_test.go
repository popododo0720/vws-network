package network

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/popododo0720/vws-network/internal/domain"
)

func TestServiceAgainstStores(t *testing.T) {
	openers := []struct {
		name string
		open func(t *testing.T) repository
	}{
		{name: "memory", open: func(t *testing.T) repository { return newMemRepo() }},
		{name: "sqlite", open: func(t *testing.T) repository {
			t.Helper()
			repo, err := openSQLite(context.Background(), filepath.Join(t.TempDir(), "net.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = repo.close() })
			return repo
		}},
	}
	for _, opener := range openers {
		t.Run(opener.name, func(t *testing.T) {
			t.Run("overlap writes nothing", func(t *testing.T) { testOverlapWritesNothing(t, opener.open(t)) })
			t.Run("idempotency", func(t *testing.T) { testIdempotency(t, opener.open(t)) })
			t.Run("converge and delete", func(t *testing.T) { testConvergeAndDelete(t, opener.open(t)) })
			t.Run("elastic ip", func(t *testing.T) { testElasticIP(t, opener.open(t)) })
			t.Run("dataplane retry", func(t *testing.T) { testDataplaneRetry(t, opener.open(t)) })
			t.Run("revoke retries after failure", func(t *testing.T) { testRevokeRetriesAfterFailure(t, opener.open(t)) })
			t.Run("delete children of failed vpc", func(t *testing.T) { testDeleteChildrenOfFailedVPC(t, opener.open(t)) })
			t.Run("cross vpc peer", func(t *testing.T) { testCrossVPCPeerIsNotFound(t, opener.open(t)) })
			t.Run("other region leaves the vpc", func(t *testing.T) { testOtherRegionLeavesVPC(t, opener.open(t)) })
		})
	}
}

func testOverlapWritesNothing(t *testing.T, repo repository) {
	t.Helper()
	svc, fake := openTest(t, repo)
	ctx := context.Background()
	if _, err := svc.CreateVPC(ctx, call("proj", "a"), domain.CreateVPC{CIDR: mustP("10.0.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.CreateVPC(ctx, call("proj", "b"), domain.CreateVPC{CIDR: mustP("10.0.1.0/24")})
	if kind(err) != domain.Conflict {
		t.Fatalf("overlap = %v", err)
	}
	if _, err := svc.CreateVPC(ctx, call("proj", "b"), domain.CreateVPC{CIDR: mustP("10.1.0.0/16")}); err != nil {
		t.Fatalf("key consumed by rejected overlap: %v", err)
	}
	if _, err := svc.CreateVPC(ctx, call("other", "a"), domain.CreateVPC{CIDR: mustP("10.0.0.0/16")}); err != nil {
		t.Fatalf("other project overlap = %v", err)
	}
	if len(fake.log()) != 0 {
		t.Fatal("validation called the dataplane")
	}
}

func testIdempotency(t *testing.T, repo repository) {
	t.Helper()
	svc, _ := openTest(t, repo)
	ctx := context.Background()
	first, err := svc.CreateVPC(ctx, call("proj", "a"), domain.CreateVPC{CIDR: mustP("10.4.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.CreateVPC(ctx, call("proj", "a"), domain.CreateVPC{CIDR: mustP("10.4.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("replay %s then %s", first.ID, second.ID)
	}
	if _, err := svc.CreateVPC(ctx, call("proj", "a"), domain.CreateVPC{CIDR: mustP("10.5.0.0/16")}); kind(err) != domain.Conflict {
		t.Fatalf("different body = %v", err)
	}
	gone, err := svc.DeleteVPC(ctx, call("proj", "missing"), domain.DeleteVPC{VPC: "vpc-does-not-exist"})
	if err != nil || gone.State != domain.OpSucceeded {
		t.Fatalf("absent delete %+v err %v", gone, err)
	}
}

func testConvergeAndDelete(t *testing.T, repo repository) {
	t.Helper()
	svc, fake := openTest(t, repo)
	ctx := context.Background()
	op, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.8.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	op, err = svc.GetOperation(ctx, "proj", op.ID)
	if err != nil || op.State != domain.OpSucceeded {
		t.Fatalf("op %+v err %v", op, err)
	}
	vpc, err := svc.GetVPC(ctx, "proj", domain.VPCID(op.Resource))
	if err != nil || vpc.Status != domain.StatusAvailable {
		t.Fatalf("vpc %+v err %v", vpc, err)
	}
	if !fake.has("vpc/"+string(vpc.ID)) || !fake.has("sg/"+string(vpc.DefaultGroup)) {
		t.Fatal("dataplane missing vpc or default group")
	}
	groups, err := svc.ListSecurityGroups(ctx, "proj", vpc.ID)
	if err != nil || len(groups) != 1 || !groups[0].Default || len(groups[0].Rules) != 1 {
		t.Fatalf("groups %+v err %v", groups, err)
	}
	sub, err := svc.CreateSubnet(ctx, call("proj", "sub"), domain.CreateSubnet{VPC: vpc.ID, CIDR: mustP("10.8.1.0/24"), Zone: "az1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeleteVPC(ctx, call("proj", "del"), domain.DeleteVPC{VPC: vpc.ID}); kind(err) != domain.Precondition {
		t.Fatalf("delete vpc with subnet = %v", err)
	}
	if _, err := svc.DeleteSubnet(ctx, call("proj", "del-sub"), domain.DeleteSubnet{Subnet: domain.SubnetID(sub.Resource)}); err != nil {
		t.Fatal(err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeleteVPC(ctx, call("proj", "del"), domain.DeleteVPC{VPC: vpc.ID}); err != nil {
		t.Fatal(err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetVPC(ctx, "proj", vpc.ID); kind(err) != domain.NotFound {
		t.Fatalf("deleted vpc err = %v", err)
	}
	if fake.has("vpc/" + string(vpc.ID)) {
		t.Fatal("dataplane kept the vpc")
	}
}

func testElasticIP(t *testing.T, repo repository) {
	t.Helper()
	svc, fake := openTest(t, repo)
	ctx := context.Background()
	vpcOp, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.9.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSubnet(ctx, call("proj", "sub"), domain.CreateSubnet{VPC: domain.VPCID(vpcOp.Resource), CIDR: mustP("10.9.0.0/24"), Zone: "az1"}); err != nil {
		t.Fatal(err)
	}
	eip, err := svc.AllocateElasticIP(ctx, call("proj", "eip"), domain.AllocateElasticIP{VPC: domain.VPCID(vpcOp.Resource)})
	if err != nil {
		t.Fatal(err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetElasticIP(ctx, "proj", domain.ElasticIPID(eip.Resource))
	if err != nil {
		t.Fatal(err)
	}
	if got.Address != netip.MustParseAddr("198.51.100.1") {
		t.Fatalf("address %v", got.Address)
	}
	if _, err := svc.AssociateElasticIP(ctx, call("proj", "assoc-bad"), domain.AssociateElasticIP{ElasticIP: got.ID, Private: netip.MustParseAddr("10.9.0.1")}); kind(err) != domain.Precondition {
		t.Fatalf("reserved host = %v", err)
	}
	if _, err := svc.AssociateElasticIP(ctx, call("proj", "assoc"), domain.AssociateElasticIP{ElasticIP: got.ID, Private: netip.MustParseAddr("10.9.0.4")}); err != nil {
		t.Fatal(err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	got, err = svc.GetElasticIP(ctx, "proj", got.ID)
	if err != nil || got.Subnet == "" || !fake.has("nat/"+string(got.ID)+"/"+got.Private.String()) {
		t.Fatalf("association %+v err %v", got, err)
	}
	if _, err := svc.ReleaseElasticIP(ctx, call("proj", "rel"), domain.ReleaseElasticIP{ElasticIP: got.ID}); kind(err) != domain.Precondition {
		t.Fatalf("release associated = %v", err)
	}
	if _, err := svc.DisassociateElasticIP(ctx, call("proj", "dis"), domain.DisassociateElasticIP{ElasticIP: got.ID}); err != nil {
		t.Fatal(err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	if fake.has("nat/" + string(got.ID) + "/10.9.0.4") {
		t.Fatal("nat remained")
	}
	if _, err := svc.ReleaseElasticIP(ctx, call("proj", "rel"), domain.ReleaseElasticIP{ElasticIP: got.ID}); err != nil {
		t.Fatal(err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	again, err := svc.AllocateElasticIP(ctx, call("proj", "eip-2"), domain.AllocateElasticIP{VPC: domain.VPCID(vpcOp.Resource)})
	if err != nil {
		t.Fatal(err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	reused, err := svc.GetElasticIP(ctx, "proj", domain.ElasticIPID(again.Resource))
	if err != nil || reused.Address != netip.MustParseAddr("198.51.100.1") {
		t.Fatalf("reused %+v err %v", reused, err)
	}
}

func testDataplaneRetry(t *testing.T, repo repository) {
	t.Helper()
	svc, fake := openTest(t, repo)
	svc.maxAttempts = 2
	svc.backoff = func(int) time.Duration { return 0 }
	fake.failNext(1, errors.New("boom"))
	ctx := context.Background()
	op, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.6.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := svc.step(ctx)
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	op, err = svc.GetOperation(ctx, "proj", op.ID)
	if err != nil || op.State != domain.OpPending {
		t.Fatalf("after failure %+v err %v", op, err)
	}
	if fake.has("vpc/" + op.Resource) {
		t.Fatal("failed ensure was recorded")
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	op, err = svc.GetOperation(ctx, "proj", op.ID)
	if err != nil || op.State != domain.OpSucceeded {
		t.Fatalf("after retry %+v err %v", op, err)
	}
}

func testRevokeRetriesAfterFailure(t *testing.T, repo repository) {
	t.Helper()
	svc, fake := openTest(t, repo)
	svc.maxAttempts = 1
	ctx := context.Background()
	op, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.14.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	vpc, err := svc.GetVPC(ctx, "proj", domain.VPCID(op.Resource))
	if err != nil {
		t.Fatal(err)
	}
	rule := domain.Rule{Direction: domain.Ingress, Protocol: domain.TCP, FromPort: 22, ToPort: 22, Peer: domain.Peer{CIDR: mustP("0.0.0.0/0")}}
	if _, err := svc.AuthorizeRules(ctx, call("proj", "auth"), domain.AuthorizeRules{Group: vpc.DefaultGroup, Rules: []domain.Rule{rule}}); err != nil {
		t.Fatal(err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	key := "rule/" + string(vpc.DefaultGroup) + "/" + rule.Identity()
	if !fake.has(key) {
		t.Fatal("authorize did not install the rule")
	}
	fake.failNext(1, errors.New("boom"))
	rev, err := svc.RevokeRules(ctx, call("proj", "revoke"), domain.RevokeRules{Group: vpc.DefaultGroup, Rules: []domain.Rule{rule}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.step(ctx); err != nil {
		t.Fatal(err)
	}
	rev, err = svc.GetOperation(ctx, "proj", rev.ID)
	if err != nil || rev.State != domain.OpFailed {
		t.Fatalf("revoke %+v err %v", rev, err)
	}
	group, err := svc.GetSecurityGroup(ctx, "proj", vpc.DefaultGroup)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range group.Rules {
		if got == rule {
			t.Fatal("revoked rule is still visible")
		}
	}
	if !fake.has(key) {
		t.Fatal("failed revoke removed the dataplane rule")
	}
	if _, err := svc.RevokeRules(ctx, call("proj", "revoke-2"), domain.RevokeRules{Group: vpc.DefaultGroup, Rules: []domain.Rule{rule}}); err != nil {
		t.Fatal(err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	if fake.has(key) {
		t.Fatal("retry left the dataplane rule")
	}
}

func testDeleteChildrenOfFailedVPC(t *testing.T, repo repository) {
	t.Helper()
	svc, fake := openTest(t, repo)
	svc.maxAttempts = 1
	fake.failNext(1, errors.New("boom"))
	ctx := context.Background()
	vpcOp, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.12.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	vpcID := domain.VPCID(vpcOp.Resource)
	subOp, err := svc.CreateSubnet(ctx, call("proj", "sub"), domain.CreateSubnet{VPC: vpcID, CIDR: mustP("10.12.1.0/24"), Zone: "az1"})
	if err != nil {
		t.Fatal(err)
	}
	eipOp, err := svc.AllocateElasticIP(ctx, call("proj", "eip"), domain.AllocateElasticIP{VPC: vpcID})
	if err != nil {
		t.Fatal(err)
	}
	held, err := svc.GetElasticIP(ctx, "proj", domain.ElasticIPID(eipOp.Resource))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.step(ctx); err != nil {
		t.Fatal(err)
	}
	vpcOp, err = svc.GetOperation(ctx, "proj", vpcOp.ID)
	if err != nil || vpcOp.State != domain.OpFailed {
		t.Fatalf("vpc op %+v err %v", vpcOp, err)
	}
	if _, err := svc.DeleteSubnet(ctx, call("proj", "del-sub"), domain.DeleteSubnet{Subnet: domain.SubnetID(subOp.Resource)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReleaseElasticIP(ctx, call("proj", "rel"), domain.ReleaseElasticIP{ElasticIP: domain.ElasticIPID(eipOp.Resource)}); err != nil {
		t.Fatal(err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetSubnet(ctx, "proj", domain.SubnetID(subOp.Resource)); kind(err) != domain.NotFound {
		t.Fatalf("subnet err = %v", err)
	}
	if _, err := svc.GetElasticIP(ctx, "proj", domain.ElasticIPID(eipOp.Resource)); kind(err) != domain.NotFound {
		t.Fatalf("eip err = %v", err)
	}
	other, err := svc.CreateVPC(ctx, call("other", "vpc"), domain.CreateVPC{CIDR: mustP("10.13.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	again, err := svc.AllocateElasticIP(ctx, call("other", "eip"), domain.AllocateElasticIP{VPC: domain.VPCID(other.Resource)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetElasticIP(ctx, "other", domain.ElasticIPID(again.Resource))
	if err != nil || got.Address != held.Address {
		t.Fatalf("reused %+v want %v err %v", got, held.Address, err)
	}
	if _, err := svc.DeleteVPC(ctx, call("proj", "del-vpc"), domain.DeleteVPC{VPC: vpcID}); err != nil {
		t.Fatal(err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetVPC(ctx, "proj", vpcID); kind(err) != domain.NotFound {
		t.Fatalf("deleted vpc err = %v", err)
	}
}

func testCrossVPCPeerIsNotFound(t *testing.T, repo repository) {
	t.Helper()
	svc, _ := openTest(t, repo)
	ctx := context.Background()
	a, err := svc.CreateVPC(ctx, call("proj", "a"), domain.CreateVPC{CIDR: mustP("10.30.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.CreateVPC(ctx, call("proj", "b"), domain.CreateVPC{CIDR: mustP("10.31.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	peer, err := svc.CreateSecurityGroup(ctx, call("proj", "peer"), domain.CreateSecurityGroup{VPC: domain.VPCID(b.Resource), Name: "peer"})
	if err != nil {
		t.Fatal(err)
	}
	vpc, err := svc.GetVPC(ctx, "proj", domain.VPCID(a.Resource))
	if err != nil {
		t.Fatal(err)
	}
	rule := domain.Rule{Direction: domain.Ingress, Protocol: domain.TCP, FromPort: 22, ToPort: 22, Peer: domain.Peer{Group: domain.SecurityGroupID(peer.Resource)}}
	_, err = svc.AuthorizeRules(ctx, call("proj", "auth"), domain.AuthorizeRules{Group: vpc.DefaultGroup, Rules: []domain.Rule{rule}})
	if kind(err) != domain.NotFound {
		t.Fatalf("cross vpc peer = %v", err)
	}
}

func TestPeerGroupEnsuredBeforeRule(t *testing.T) {
	svc, fake := openTest(t, newMemRepo())
	ctx := context.Background()
	op, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.20.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	peer, err := svc.CreateSecurityGroup(ctx, call("proj", "peer"), domain.CreateSecurityGroup{VPC: domain.VPCID(op.Resource), Name: "peer"})
	if err != nil {
		t.Fatal(err)
	}
	vpc, err := svc.GetVPC(ctx, "proj", domain.VPCID(op.Resource))
	if err != nil {
		t.Fatal(err)
	}
	rule := domain.Rule{Direction: domain.Ingress, Protocol: domain.TCP, FromPort: 80, ToPort: 80, Peer: domain.Peer{Group: domain.SecurityGroupID(peer.Resource)}}
	if _, err := svc.AuthorizeRules(ctx, call("proj", "auth"), domain.AuthorizeRules{Group: vpc.DefaultGroup, Rules: []domain.Rule{rule}}); err != nil {
		t.Fatal(err)
	}
	ok, err := svc.step(ctx)
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	peerAt := logIndex(fake.log(), "ensure-sg/"+peer.Resource)
	ruleAt := logIndex(fake.log(), "ensure-rule/"+string(vpc.DefaultGroup)+"/"+rule.Identity())
	if peerAt < 0 || ruleAt < 0 || peerAt > ruleAt {
		t.Fatalf("peer %d rule %d log %v", peerAt, ruleAt, fake.log())
	}
}

func TestStepReleasesClaimWhenLoadFails(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	svc, err := newService(ctx, repo, newFake(), mustP("198.51.100.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	op, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.21.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	svc.repo = &failLoad{repository: repo, n: 1}
	ok, err := svc.step(ctx)
	if err == nil || ok {
		t.Fatalf("step ok %v err %v", ok, err)
	}
	op, err = svc.GetOperation(ctx, "proj", op.ID)
	if err != nil || op.State != domain.OpPending {
		t.Fatalf("after failed load %+v err %v", op, err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	op, err = svc.GetOperation(ctx, "proj", op.ID)
	if err != nil || op.State != domain.OpSucceeded {
		t.Fatalf("after release %+v err %v", op, err)
	}
}

func TestRetryVPCDeleteDoesNotCreateGhostGroup(t *testing.T) {
	svc, _ := openTest(t, newMemRepo())
	ctx := context.Background()
	op, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.22.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	vpc, err := svc.GetVPC(ctx, "proj", domain.VPCID(op.Resource))
	if err != nil {
		t.Fatal(err)
	}
	rule := domain.Rule{Direction: domain.Ingress, Protocol: domain.TCP, FromPort: 443, ToPort: 443, Peer: domain.Peer{CIDR: mustP("192.0.2.0/24")}}
	if _, err := svc.AuthorizeRules(ctx, call("proj", "auth"), domain.AuthorizeRules{Group: vpc.DefaultGroup, Rules: []domain.Rule{rule}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeleteVPC(ctx, call("proj", "del"), domain.DeleteVPC{VPC: vpc.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.step(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetSecurityGroup(ctx, "proj", vpc.DefaultGroup); kind(err) != domain.NotFound {
		t.Fatalf("default group err = %v", err)
	}
	if _, err := svc.DeleteVPC(ctx, call("proj", "del-2"), domain.DeleteVPC{VPC: vpc.ID}); err != nil {
		t.Fatal(err)
	}
	groups, err := svc.ListSecurityGroups(ctx, "proj", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g.ID == "" {
			t.Fatal("second delete stored an empty security group")
		}
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetVPC(ctx, "proj", vpc.ID); kind(err) != domain.NotFound {
		t.Fatalf("vpc err = %v", err)
	}
}

func TestMutateExhaustionIsDomainError(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		err  error
		kind domain.ErrKind
	}{
		{name: "stale", err: errStale, kind: domain.Busy},
		{name: "address", err: errAddressTaken, kind: domain.Conflict},
		{name: "key", err: errKeyTaken, kind: domain.Busy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &failCommit{repository: newMemRepo(), err: tc.err}
			svc, err := newService(ctx, repo, newFake(), mustP("198.51.100.0/24"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.23.0.0/16")})
			var de *domain.Error
			if !errors.As(err, &de) || de.Kind != tc.kind || errors.Is(err, tc.err) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

type failCommit struct {
	repository
	err error
}

func (f *failCommit) commit(ctx context.Context, c commit) error {
	if c.enqueue != nil {
		return f.err
	}
	return f.repository.commit(ctx, c)
}

func TestFakeNilFailureIsNotLostClaim(t *testing.T) {
	fake := newFake()
	fake.failNext(1, nil)
	err := fake.ensureVPC(context.Background(), domain.VPC{ID: "vpc-1"})
	if err == nil || errors.Is(err, errLostClaim) {
		t.Fatalf("fail error = %v", err)
	}
}

type failLoad struct {
	repository
	n int
}

func (f *failLoad) load(ctx context.Context, p domain.ProjectID) (domain.State, int64, error) {
	if f.n > 0 {
		f.n--
		return domain.State{}, 0, errors.New("load failed")
	}
	return f.repository.load(ctx, p)
}

func logIndex(calls []string, name string) int {
	for i, c := range calls {
		if c == name {
			return i
		}
	}
	return -1
}

func TestRestartRequeuesRunningOperation(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	fake := newFake()
	first, err := newService(ctx, repo, fake, mustP("198.51.100.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	op, err := first.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.7.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repo.claim(ctx, first.now(), "crash"); err != nil || !ok {
		t.Fatal(err, ok)
	}
	second, err := newService(ctx, repo, fake, mustP("198.51.100.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	if err := drain(second); err != nil {
		t.Fatal(err)
	}
	op, err = second.GetOperation(ctx, "proj", op.ID)
	if err != nil || op.State != domain.OpSucceeded {
		t.Fatalf("after restart %+v err %v", op, err)
	}
	if !fake.has("vpc/" + op.Resource) {
		t.Fatal("restart did not converge")
	}
}

func TestSQLiteSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "net.db")
	repo, err := openSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := newService(ctx, repo, newFake(), mustP("198.51.100.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	op, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.3.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	fake := newFake()
	again, err := newService(ctx, reopened, fake, mustP("198.51.100.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	if err := drain(again); err != nil {
		t.Fatal(err)
	}
	op, err = again.GetOperation(ctx, "proj", op.ID)
	if err != nil || op.State != domain.OpSucceeded {
		t.Fatalf("reopened %+v err %v", op, err)
	}
	vpc, err := again.GetVPC(ctx, "proj", domain.VPCID(op.Resource))
	if err != nil || vpc.Status != domain.StatusAvailable || vpc.CIDR != mustP("10.3.0.0/16") {
		t.Fatalf("vpc %+v err %v", vpc, err)
	}
}

func TestAttemptsExhaustedMarksFailed(t *testing.T) {
	ctx := context.Background()
	fake := newFake()
	fake.failNext(5, errors.New("boom"))
	svc, err := newService(ctx, newMemRepo(), fake, mustP("198.51.100.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	svc.maxAttempts = 1
	op, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.11.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.step(ctx); err != nil {
		t.Fatal(err)
	}
	op, err = svc.GetOperation(ctx, "proj", op.ID)
	if err != nil || op.State != domain.OpFailed || op.Err == nil || op.Err.Kind != domain.Dataplane {
		t.Fatalf("op %+v err %v", op, err)
	}
	vpc, err := svc.GetVPC(ctx, "proj", domain.VPCID(op.Resource))
	if err != nil || vpc.Status != domain.StatusFailed {
		t.Fatalf("vpc %+v err %v", vpc, err)
	}
}

func testOtherRegionLeavesVPC(t *testing.T, repo repository) {
	t.Helper()
	svc, _ := openTest(t, repo)
	ctx := context.Background()
	if _, err := svc.CreateVPC(ctx, call("proj", "a"), domain.CreateVPC{CIDR: mustP("10.8.0.0/16"), ID: "app", Region: "r1"}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.DeleteVPC(ctx, call("proj", "b"), domain.DeleteVPC{VPC: "app", Region: "r2", RejectMissing: true})
	if kind(err) != domain.NotFound {
		t.Fatalf("delete other region = %v", err)
	}
	if _, err := svc.UpdateVPC(ctx, call("proj", "c"), domain.CreateVPC{CIDR: mustP("10.8.0.0/16"), ID: "app", Region: "r2"}); kind(err) != domain.NotFound {
		t.Fatalf("update other region = %v", err)
	}
	if _, err := svc.GetVPC(ctx, "proj", "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeleteVPC(ctx, call("proj", "d"), domain.DeleteVPC{VPC: "app", Region: "r2"}); err != nil {
		t.Fatalf("allow missing other region = %v", err)
	}
	if _, err := svc.GetVPC(ctx, "proj", "app"); err != nil {
		t.Fatal(err)
	}
}

func openTest(t *testing.T, repo repository) (*Service, *fakeDataplane) {
	t.Helper()
	fake := newFake()
	svc, err := newService(context.Background(), repo, fake, mustP("198.51.100.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	svc.backoff = func(int) time.Duration { return 0 }
	return svc, fake
}

func drain(svc *Service) error {
	for i := 0; i < 20; i++ {
		ok, err := svc.step(context.Background())
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	return errors.New("drain did not finish")
}

func call(project, key string) domain.Call {
	return domain.Call{Project: domain.ProjectID(project), Key: domain.IdempotencyKey(key)}
}

func mustP(s string) netip.Prefix { return netip.MustParsePrefix(s) }

func kind(err error) domain.ErrKind {
	var e *domain.Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return 0
}
