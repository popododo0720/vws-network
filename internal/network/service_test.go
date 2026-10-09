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
