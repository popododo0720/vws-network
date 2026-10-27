//go:build gen

package network

import (
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/popododo0720/vws-network/internal/domain"
)

// TestGenerateUpgradeFixtures writes testdata/upgrade/<build>/ from this build.
// It is the only writer of those files. A normal test run never compiles it.
// Regenerate at a release bump with:
//
//	UPGRADE_FIXTURE_VERSION=<tag or sha> go test -tags gen -run TestGenerateUpgradeFixtures ./internal/network
func TestGenerateUpgradeFixtures(t *testing.T) {
	version := os.Getenv("UPGRADE_FIXTURE_VERSION")
	if version == "" {
		t.Skip("set UPGRADE_FIXTURE_VERSION to the released build (tag or sha) to write its fixtures")
	}
	dir := filepath.Join("testdata", "upgrade", version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeUpgradeDatabase(t, filepath.Join(dir, "net.db"))
	writeUpgradeObjects(t, dir)
}

// upgradeFixtureState is the one project the fixtures hold.
func upgradeFixtureState(t *testing.T) domain.State {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var st domain.State
	must(st.CreateVPC("vpc-1", "sg-1", domain.CreateVPC{
		CIDR: mustP("10.8.0.0/16"), Region: "r1",
		Text: domain.Text{Description: "upgrade fixture vpc", Labels: map[string]string{"env": "upgrade"}},
	}))
	must(st.CreateSubnet("subnet-1", domain.CreateSubnet{
		VPC: "vpc-1", CIDR: mustP("10.8.1.0/24"), Zone: "az1", Region: "r1",
		Text: domain.Text{Description: "upgrade fixture subnet", Labels: map[string]string{"tier": "app"}},
	}))
	must(st.CreateSecurityGroup("sg-2", domain.CreateSecurityGroup{
		VPC: "vpc-1", Name: "web", Region: "r1",
		Text: domain.Text{Description: "upgrade fixture group", Labels: map[string]string{"role": "web"}},
	}))
	ingress := domain.Rule{Direction: domain.Ingress, Protocol: domain.TCP, FromPort: 80, ToPort: 80, Peer: domain.Peer{CIDR: mustP("0.0.0.0/0")}}
	peer := domain.Rule{Direction: domain.Egress, Protocol: domain.All, FromPort: -1, ToPort: -1, Peer: domain.Peer{Group: "sg-1"}}
	dns := domain.Rule{Direction: domain.Ingress, Protocol: domain.UDP, FromPort: 53, ToPort: 53, Peer: domain.Peer{CIDR: mustP("10.8.1.0/24")}}
	must(st.AuthorizeRules(domain.AuthorizeRules{Group: "sg-2", Rules: []domain.Rule{ingress, peer, dns}}))
	revoked, err := st.RevokeRules(domain.RevokeRules{Group: "sg-2", Rules: []domain.Rule{dns}})
	must(err)
	if !revoked {
		t.Fatal("the dns rule was not moved to revoking")
	}
	must(st.AllocateElasticIP("eipalloc-1", domain.AllocateElasticIP{
		VPC: "vpc-1", Region: "r1", Labels: map[string]string{"app": "web"},
	}, mustP("198.51.100.0/24"), func(netip.Addr) bool { return false }))
	associated, err := st.SetElasticIPAssociation("eipalloc-1", "i-0123456789abcdef0", "eth0")
	must(err)
	if !associated {
		t.Fatal("the elastic ip was not attached")
	}
	must(st.BindInterfaceAddress("eipalloc-1", netip.MustParseAddr("10.8.1.10")))
	moved, err := st.MoveInterfaceAddress("eipalloc-1", netip.MustParseAddr("10.8.1.11"))
	must(err)
	if !moved {
		t.Fatal("the old private address did not move to detaching")
	}
	for id, v := range st.VPCs {
		v.Status = domain.StatusAvailable
		st.VPCs[id] = v
	}
	for id, v := range st.Subnets {
		v.Status = domain.StatusAvailable
		st.Subnets[id] = v
	}
	for id, v := range st.Groups {
		v.Status = domain.StatusAvailable
		st.Groups[id] = v
	}
	for id, v := range st.ElasticIPs {
		v.Status = domain.StatusAvailable
		st.ElasticIPs[id] = v
	}
	return st
}

func writeUpgradeDatabase(t *testing.T, path string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	repo, err := openSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	st := upgradeFixtureState(t)
	if err := repo.commit(ctx, commit{project: upgradeFixtureProject, state: &st}); err != nil {
		t.Fatal(err)
	}
	seedUpgradeOperations(t, ctx, repo)
	if _, err := repo.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	closeOrFail(t, repo.close)
	if _, err := os.Stat(path + "-wal"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("write-ahead log was not checkpointed: %v", err)
	}
}

// seedUpgradeOperations leaves one succeeded, one failed and one pending
// operation, each with a key and fingerprint, so the fixture covers every
// non-terminal and terminal row the repository reads.
func seedUpgradeOperations(t *testing.T, ctx context.Context, repo *sqliteRepo) {
	t.Helper()
	enqueue := func(op domain.Operation, ts time.Time, attempts int, seed string) job {
		t.Helper()
		j := job{op: op, lane: "vpc-1", fingerprint: sha256.Sum256([]byte(seed)), attempts: attempts, notBefore: ts}
		if err := repo.commit(ctx, commit{project: op.Project, enqueue: &j}); err != nil {
			t.Fatal(err)
		}
		return j
	}
	settle := func(j job, state domain.OpState, opErr *domain.Error) {
		t.Helper()
		claimed, ok, err := repo.claim(ctx, upgradeClaimAt, "claim-"+j.op.ID)
		if err != nil || !ok {
			t.Fatalf("claim %s: ok %v err %v", j.op.ID, ok, err)
		}
		done := claimed
		done.op.State = state
		done.op.Err = opErr
		if err := repo.commit(ctx, commit{project: done.op.Project, settle: &done}); err != nil {
			t.Fatal(err)
		}
	}
	settle(enqueue(domain.Operation{
		ID: "op-succeeded", Project: upgradeFixtureProject, Type: domain.OpAuthorizeRules,
		Resource: "sg-2", State: domain.OpPending, Key: "key-succeeded",
	}, upgradeClaimAt.Add(-2*time.Hour), 1, upgradeFingerprintSucceeded), domain.OpSucceeded, nil)
	settle(enqueue(domain.Operation{
		ID: "op-failed", Project: upgradeFixtureProject, Type: domain.OpCreateSubnet,
		Resource: "subnet-1", State: domain.OpPending, Key: "key-failed",
	}, upgradeClaimAt.Add(-time.Hour), 3, upgradeFingerprintFailed), domain.OpFailed,
		&domain.Error{Kind: domain.Conflict, Msg: "subnet subnet-1 has a different cidr"})
	enqueue(domain.Operation{
		ID: "op-pending", Project: upgradeFixtureProject, Type: domain.OpAllocateElasticIP,
		Resource: "eipalloc-1", State: domain.OpPending, Key: "key-pending",
	}, upgradeClaimAt.Add(time.Hour), 0, upgradeFingerprintPending)
}

// writeUpgradeObjects runs ensure for the fixture state, then writes what the
// cluster holds with the fields the API server owns stripped.
func writeUpgradeObjects(t *testing.T, dir string) {
	t.Helper()
	ctx := context.Background()
	st := upgradeFixtureState(t)
	k, c := newKube()
	k.snat = true
	k.vmMTU = DefaultVMMTU
	if err := k.ensureVPC(ctx, st.VPCs["vpc-1"]); err != nil {
		t.Fatal(err)
	}
	for _, id := range []domain.SecurityGroupID{"sg-1", "sg-2"} {
		g := st.Groups[id]
		if err := k.ensureGroup(ctx, g); err != nil {
			t.Fatal(err)
		}
		for _, r := range g.Rules {
			if err := k.ensureRule(ctx, g.ID, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := k.ensureSubnet(ctx, st.Subnets["subnet-1"]); err != nil {
		t.Fatal(err)
	}
	eip := st.ElasticIPs["eipalloc-1"]
	if err := k.ensureElasticIP(ctx, eip); err != nil {
		t.Fatal(err)
	}
	if err := k.ensureNAT(ctx, eip, eip.Private); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []struct {
		gvk  schema.GroupVersionKind
		ns   string
		name string
	}{
		{gvkVpc, "", "vpc-1"},
		{gvkSubnet, "", "subnet-1"},
		{gvkNAD, k.nadNamespace, nadName("subnet-1")},
		{gvkGroup, "", "sg-1"},
		{gvkGroup, "", "sg-2"},
		{gvkEip, "", "eipalloc-1"},
		{gvkFip, "", fipName("eipalloc-1", eip.Private)},
		{gvkEip, "", snatName("subnet-1")},
		{gvkSnat, "", snatName("subnet-1")},
	} {
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(ref.gvk)
		if err := c.Get(ctx, client.ObjectKey{Namespace: ref.ns, Name: ref.name}, o); err != nil {
			t.Fatal(err)
		}
		writeUpgradeObject(t, dir, o)
	}
}

func writeUpgradeObject(t *testing.T, dir string, o *unstructured.Unstructured) {
	t.Helper()
	md := map[string]any{"name": o.GetName()}
	if ns := o.GetNamespace(); ns != "" {
		md["namespace"] = ns
	}
	if annotations := o.GetAnnotations(); len(annotations) > 0 {
		md["annotations"] = annotations
	}
	o.Object["metadata"] = md
	delete(o.Object, "status")
	data, err := yaml.Marshal(o.Object)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.ToLower(o.GetKind()) + "-" + o.GetName() + ".yaml"
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
