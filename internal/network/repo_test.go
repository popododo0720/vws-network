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

func TestPickRunnableHonorsLaneAndTime(t *testing.T) {
	now := time.Unix(100, 0)
	jobs := []job{
		{seq: 1, lane: "vpc-a", op: domain.Operation{State: domain.OpRunning}},
		{seq: 2, lane: "vpc-a", op: domain.Operation{State: domain.OpPending, ID: "later"}},
		{seq: 3, lane: "vpc-b", notBefore: now.Add(time.Second), op: domain.Operation{State: domain.OpPending, ID: "wait"}},
		{seq: 4, lane: "vpc-b", op: domain.Operation{State: domain.OpPending, ID: "blocked"}},
		{seq: 5, lane: "vpc-c", op: domain.Operation{State: domain.OpPending, ID: "ready"}},
	}
	got, ok := pickRunnable(jobs, now)
	if !ok || got.op.ID != "ready" {
		t.Fatalf("got %+v ok %v", got.op, ok)
	}
}

func TestRepositoryContract(t *testing.T) {
	ctx := context.Background()
	openers := []struct {
		name string
		open func(t *testing.T) repository
	}{
		{name: "memory", open: func(t *testing.T) repository { return newMemRepo() }},
		{name: "sqlite", open: func(t *testing.T) repository {
			t.Helper()
			repo, err := openSQLite(ctx, filepath.Join(t.TempDir(), "net.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = repo.close() })
			return repo
		}},
	}
	for _, opener := range openers {
		t.Run(opener.name, func(t *testing.T) {
			repo := opener.open(t)
			addr := netip.MustParseAddr("198.51.100.9")
			st := domain.State{ElasticIPs: map[domain.ElasticIPID]domain.ElasticIP{
				"eip-1": {ID: "eip-1", Address: addr, Status: domain.StatusPending},
			}}
			op := domain.Operation{ID: "op-1", Project: "proj", Type: domain.OpAllocateElasticIP, Resource: "eip-1", State: domain.OpPending, Key: "k"}
			err := repo.commit(ctx, commit{
				project: "proj", base: 0, state: &st,
				enqueue: &job{op: op, lane: "vpc-1"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.commit(ctx, commit{project: "proj", base: 0, state: &st, enqueue: &job{op: op}}); !errors.Is(err, errStale) {
				t.Fatalf("second commit = %v", err)
			}
			other := st.Clone()
			err = repo.commit(ctx, commit{project: "other", base: 0, state: &other, enqueue: &job{op: domain.Operation{ID: "op-2", Project: "other", Key: "k2", State: domain.OpPending}}})
			if !errors.Is(err, errAddressTaken) {
				t.Fatalf("cross-project address = %v", err)
			}
			claimed, ok, err := repo.claim(ctx, time.Now(), "tok")
			if err != nil || !ok || claimed.op.State != domain.OpRunning {
				t.Fatalf("claim %+v ok %v err %v", claimed.op, ok, err)
			}
			if _, ok, err := repo.claim(ctx, time.Now(), "tok-2"); err != nil || ok {
				t.Fatalf("second claim ok %v err %v", ok, err)
			}
			if err := repo.requeue(ctx); err != nil {
				t.Fatal(err)
			}
			again, ok, err := repo.claim(ctx, time.Now(), "tok-3")
			if err != nil || !ok || again.token != "tok-3" {
				t.Fatalf("reclaim %+v ok %v err %v", again, ok, err)
			}
		})
	}
}

func TestSettleTokenGuardsState(t *testing.T) {
	ctx := context.Background()
	cidr := netip.MustParsePrefix("10.0.0.0/16")
	openers := []struct {
		name string
		open func(t *testing.T) repository
	}{
		{name: "memory", open: func(t *testing.T) repository { return newMemRepo() }},
		{name: "sqlite", open: func(t *testing.T) repository {
			t.Helper()
			repo, err := openSQLite(ctx, filepath.Join(t.TempDir(), "net.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = repo.close() })
			return repo
		}},
	}
	for _, opener := range openers {
		t.Run(opener.name, func(t *testing.T) {
			repo := opener.open(t)
			st := domain.State{VPCs: map[domain.VPCID]domain.VPC{
				"vpc-1": {ID: "vpc-1", CIDR: cidr, Status: domain.StatusPending},
			}}
			op := domain.Operation{ID: "op-1", Project: "proj", Type: domain.OpCreateVPC, Resource: "vpc-1", State: domain.OpPending, Key: "k"}
			if err := repo.commit(ctx, commit{project: "proj", base: 0, state: &st, enqueue: &job{op: op, lane: "vpc-1"}}); err != nil {
				t.Fatal(err)
			}
			_, ver, err := repo.load(ctx, "proj")
			if err != nil {
				t.Fatal(err)
			}
			claimed, ok, err := repo.claim(ctx, time.Now(), "tok")
			if err != nil || !ok {
				t.Fatal(err, ok)
			}
			mut := st.Clone()
			mut.Subnets = map[domain.SubnetID]domain.Subnet{
				"subnet-1": {ID: "subnet-1", VPC: "vpc-1", CIDR: netip.MustParsePrefix("10.0.1.0/24"), Zone: "az1", Status: domain.StatusPending},
			}
			bad := claimed
			bad.token = "other"
			bad.op.State = domain.OpSucceeded
			if err := repo.commit(ctx, commit{project: "proj", base: ver, state: &mut, settle: &bad}); !errors.Is(err, errLostClaim) {
				t.Fatalf("wrong token = %v", err)
			}
			got, ver2, err := repo.load(ctx, "proj")
			if err != nil || ver2 != ver || len(got.Subnets) != 0 {
				t.Fatalf("wrong token wrote state ver %d subnets %d err %v", ver2, len(got.Subnets), err)
			}
			done := claimed
			done.op.State = domain.OpSucceeded
			if err := repo.commit(ctx, commit{project: "proj", base: ver, state: &st, settle: &done}); err != nil {
				t.Fatal(err)
			}
			_, ver3, err := repo.load(ctx, "proj")
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.commit(ctx, commit{project: "proj", base: ver3, state: &mut, settle: &done}); err != nil {
				t.Fatalf("terminal settle = %v", err)
			}
			got, ver4, err := repo.load(ctx, "proj")
			if err != nil || ver4 != ver3 || len(got.Subnets) != 0 {
				t.Fatalf("terminal settle wrote state ver %d/%d subnets %d err %v", ver4, ver3, len(got.Subnets), err)
			}
		})
	}
}

func TestSQLiteSchemaIsOneVersion(t *testing.T) {
	ctx := context.Background()
	repo, err := openSQLite(ctx, filepath.Join(t.TempDir(), "net.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.close() })
	var version, timeout int
	var mode string
	if err := repo.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("user_version = %d", version)
	}
	if err := repo.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %s", mode)
	}
	if err := repo.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout != 5000 {
		t.Fatalf("busy_timeout = %d", timeout)
	}
}
