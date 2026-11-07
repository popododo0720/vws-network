package network

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/popododo0720/vws-network/internal/domain"
)

var errInjected = errors.New("injected crash")

// crashAfterHead fails every write after the first head write, as if the
// process died between swapping the head and applying its change set.
type crashAfterHead struct {
	client.Client
	headWritten bool
}

func (c *crashAfterHead) stop(obj client.Object) error {
	if c.headWritten {
		return errInjected
	}
	if obj.GetLabels()[labelStore] == storeHead {
		c.headWritten = true
	}
	return nil
}

func (c *crashAfterHead) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if err := c.stop(obj); err != nil {
		return err
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *crashAfterHead) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if err := c.stop(obj); err != nil {
		return err
	}
	return c.Client.Update(ctx, obj, opts...)
}

func eipState(id domain.ElasticIPID, addr string) domain.State {
	return domain.State{ElasticIPs: map[domain.ElasticIPID]domain.ElasticIP{
		id: {ID: id, Address: netip.MustParseAddr(addr), Status: domain.StatusPending},
	}}
}

func TestKubeRepoFinishesChangeAfterCrash(t *testing.T) {
	ctx := context.Background()
	base := fake.NewClientBuilder().Build()
	crashing := openKubeRepo(t, &crashAfterHead{Client: base}, "vws-system")
	st := eipState("eip-1", "198.51.100.7")
	op := domain.Operation{
		ID: "op-1", Project: "proj", Type: domain.OpAllocateElasticIP, Resource: "eip-1", State: domain.OpPending, Key: "k",
	}
	err := crashing.commit(ctx, commit{project: "proj", state: &st, enqueue: &job{op: op, lane: "vpc-1"}})
	if !errors.Is(err, errInjected) {
		t.Fatalf("crashing commit = %v", err)
	}

	repo := openKubeRepo(t, base, "vws-system")
	got, ver, err := repo.load(ctx, "proj")
	if err != nil || ver != 1 || got.ElasticIPs["eip-1"].Address != netip.MustParseAddr("198.51.100.7") {
		t.Fatalf("after restart ver %d state %+v err %v", ver, got, err)
	}
	j, ok, err := repo.key(ctx, "proj", "k")
	if err != nil || !ok || j.op.ID != "op-1" || j.op.State != domain.OpPending {
		t.Fatalf("key after restart %+v ok %v err %v", j.op, ok, err)
	}
	used, err := repo.addressesInUse(ctx)
	if err != nil || !used[netip.MustParseAddr("198.51.100.7")] {
		t.Fatalf("addresses %v err %v", used, err)
	}
	if err := repo.commit(ctx, commit{project: "proj", enqueue: &job{op: op}}); !errors.Is(err, errKeyTaken) {
		t.Fatalf("replayed key = %v", err)
	}
}

func TestKubeRepoSnapshotIgnoresLaterChange(t *testing.T) {
	ctx := context.Background()
	repo := openKubeRepo(t, fake.NewClientBuilder().Build(), "vws-system")
	st := eipState("eip-1", "198.51.100.7")
	if err := repo.commit(ctx, commit{project: "proj", state: &st}); err != nil {
		t.Fatal(err)
	}
	_, h, _, err := repo.loadHead(ctx, "proj")
	if err != nil {
		t.Fatal(err)
	}
	next := eipState("eip-1", "198.51.100.8")
	docs, err := documents(next)
	if err != nil {
		t.Fatal(err)
	}
	ref := resRef{Kind: "eip", ID: "eip-1"}
	if err := repo.applyResource(ctx, "proj", h.seq+1, resPut{resRef: ref, State: docs[ref]}); err != nil {
		t.Fatal(err)
	}
	old, _, ok, err := repo.snapshot(ctx, "proj", h.seq)
	if err != nil || !ok || old.ElasticIPs["eip-1"].Address != netip.MustParseAddr("198.51.100.7") {
		t.Fatalf("snapshot at %d = %+v ok %v err %v", h.seq, old, ok, err)
	}
	if err := repo.applyResource(ctx, "proj", h.seq, resPut{resRef: ref}); err != nil {
		t.Fatal(err)
	}
	cur, _, ok, err := repo.snapshot(ctx, "proj", h.seq+1)
	if err != nil || !ok || cur.ElasticIPs["eip-1"].Address != netip.MustParseAddr("198.51.100.8") {
		t.Fatalf("an older change moved the resource back: %+v ok %v err %v", cur, ok, err)
	}
}

func TestKubeRepoDeleteLeavesTombstoneUntilGrace(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().Build()
	repo := openKubeRepo(t, c, "vws-system")
	st := eipState("eip-1", "198.51.100.7")
	if err := repo.commit(ctx, commit{project: "proj", state: &st}); err != nil {
		t.Fatal(err)
	}
	empty := domain.State{}
	if err := repo.commit(ctx, commit{project: "proj", base: 1, state: &empty}); err != nil {
		t.Fatal(err)
	}
	got, _, err := repo.load(ctx, "proj")
	if err != nil || len(got.ElasticIPs) != 0 {
		t.Fatalf("deleted eip still loads: %+v %v", got, err)
	}
	if used, err := repo.addressesInUse(ctx); err != nil || len(used) != 0 {
		t.Fatalf("released address still in use: %v %v", used, err)
	}
	if n := countStore(t, c, storeResource); n != 1 {
		t.Fatalf("resources %d, want the tombstone", n)
	}
	repo.now = func() time.Time { return time.Now().Add(2 * kubeGrace) }
	if err := repo.repair(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countStore(t, c, storeResource); n != 0 {
		t.Fatalf("resources %d after grace", n)
	}
}

func TestKubeRepoRepairDropsStrayAddressClaim(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().Build()
	repo := openKubeRepo(t, c, "vws-system")
	stray := repo.configMap(addrName(netip.MustParseAddr("198.51.100.9")), storeAddress, projectSet("proj"),
		map[string]string{"value": "198.51.100.9", "project": "proj", "txn": "chg-lost"})
	stray.CreationTimestamp = metav1.NewTime(time.Now())
	if err := c.Create(ctx, stray); err != nil {
		t.Fatal(err)
	}
	if err := repo.repair(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countStore(t, c, storeAddress); n != 1 {
		t.Fatalf("a fresh claim may belong to a commit in flight; claims %d", n)
	}
	repo.now = func() time.Time { return time.Now().Add(2 * kubeGrace) }
	if err := repo.repair(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countStore(t, c, storeAddress); n != 0 {
		t.Fatalf("stray claim kept: %d", n)
	}
}

func TestKubeRepoRefusesObjectOverLimit(t *testing.T) {
	repo := openKubeRepo(t, fake.NewClientBuilder().Build(), "vws-system")
	cm := repo.configMap("big", storeResource, nil, map[string]string{"state": strings.Repeat("x", kubeObjectLimit)})
	if err := repo.create(context.Background(), cm); !errors.Is(err, errObjectTooLarge) {
		t.Fatalf("create = %v", err)
	}
}

func countStore(t *testing.T, c client.Client, store string) int {
	t.Helper()
	var list corev1.ConfigMapList
	if err := c.List(context.Background(), &list, client.MatchingLabels{labelStore: store}); err != nil {
		t.Fatal(err)
	}
	return len(list.Items)
}

var errLostResponse = errors.New("connection reset")

// lostHeadResponse writes the head and then reports a failure, as when the
// API server applied a write but its answer never arrived.
type lostHeadResponse struct{ client.Client }

func (c lostHeadResponse) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if err := c.Client.Create(ctx, obj, opts...); err != nil || obj.GetLabels()[labelStore] != storeHead {
		return err
	}
	return errLostResponse
}

func TestKubeRepoKeepsClaimsWhenHeadSwapIsUncertain(t *testing.T) {
	ctx := context.Background()
	base := fake.NewClientBuilder().Build()
	lossy := openKubeRepo(t, lostHeadResponse{base}, "vws-system")
	st := eipState("eip-1", "198.51.100.7")
	if err := lossy.commit(ctx, commit{project: "proj", state: &st}); !errors.Is(err, errLostResponse) {
		t.Fatalf("commit = %v", err)
	}
	repo := openKubeRepo(t, base, "vws-system")
	other := eipState("eip-2", "198.51.100.7")
	if err := repo.commit(ctx, commit{project: "other", state: &other}); !errors.Is(err, errAddressTaken) {
		t.Fatalf("second project took a committed address: %v", err)
	}
	got, _, err := repo.load(ctx, "proj")
	if err != nil || got.ElasticIPs["eip-1"].Address != netip.MustParseAddr("198.51.100.7") {
		t.Fatalf("committed eip lost: %+v %v", got, err)
	}
}

func TestKubeRepoRefusesResourceTooLargeBeforeSwap(t *testing.T) {
	ctx := context.Background()
	repo := openKubeRepo(t, fake.NewClientBuilder().Build(), "vws-system")
	g := domain.SecurityGroup{ID: "sg-1", VPC: "vpc-1", Name: strings.Repeat("g", 140<<10)}
	st := domain.State{Groups: map[domain.SecurityGroupID]domain.SecurityGroup{"sg-1": g}}
	if err := repo.commit(ctx, commit{project: "proj", state: &st}); err != nil {
		t.Fatal(err)
	}
	g.Description = "changed"
	next := domain.State{Groups: map[domain.SecurityGroupID]domain.SecurityGroup{"sg-1": g}}
	var de *domain.Error
	err := repo.commit(ctx, commit{project: "proj", base: 1, state: &next})
	if !errors.As(err, &de) || de.Kind != domain.Invalid {
		t.Fatalf("oversized resource = %v", err)
	}
	got, ver, err := repo.load(ctx, "proj")
	if err != nil || ver != 1 || got.Groups["sg-1"].Description != "" {
		t.Fatalf("project after refused commit ver %d err %v", ver, err)
	}
}

func TestKubeRepoAllocationReclaimsStrayClaim(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().Build()
	repo := openKubeRepo(t, c, "vws-system")
	st := eipState("eip-1", "198.51.100.7")
	if err := repo.commit(ctx, commit{project: "proj", state: &st}); err != nil {
		t.Fatal(err)
	}
	stray := repo.configMap(addrName(netip.MustParseAddr("198.51.100.9")), storeAddress, projectSet("other"),
		map[string]string{"value": "198.51.100.9", "project": "other", "txn": "chg-lost"})
	if err := c.Create(ctx, stray); err != nil {
		t.Fatal(err)
	}
	repo.now = func() time.Time { return time.Now().Add(2 * kubeGrace) }
	used, err := repo.addressesInUse(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !used[netip.MustParseAddr("198.51.100.7")] || used[netip.MustParseAddr("198.51.100.9")] {
		t.Fatalf("in use after grace: %v", used)
	}
	other := eipState("eip-2", "198.51.100.9")
	if err := repo.commit(ctx, commit{project: "other", state: &other}); err != nil {
		t.Fatalf("freed address: %v", err)
	}
}

// TestKubeRepoOwnIDClaimIsStaleNotTaken: a claim on an id left by an earlier
// commit of the same project, whose head swap is uncertain, is retryable for
// that project and refused for another one until it turns stray.
func TestKubeRepoOwnIDClaimIsStaleNotTaken(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().Build()
	repo := openKubeRepo(t, c, "vws-system")
	ref := idRef(resRef{kindVPC, "vpc-1"})
	left := repo.configMap(ref.name(), storeID, projectSet("proj"),
		map[string]string{"value": ref.Value, "project": "proj", "txn": "chg-lost"})
	left.CreationTimestamp = metav1.NewTime(time.Now())
	if err := c.Create(ctx, left); err != nil {
		t.Fatal(err)
	}
	st := domain.State{VPCs: map[domain.VPCID]domain.VPC{
		"vpc-1": {ID: "vpc-1", CIDR: netip.MustParsePrefix("10.0.0.0/16"), Status: domain.StatusPending},
	}}
	if err := repo.commit(ctx, commit{project: "proj", state: &st}); !errors.Is(err, errStale) {
		t.Fatalf("own fresh claim = %v, want errStale", err)
	}
	if err := repo.commit(ctx, commit{project: "other", state: &st}); !errors.Is(err, errIDTaken) {
		t.Fatalf("another project's fresh claim = %v, want errIDTaken", err)
	}
	repo.now = func() time.Time { return time.Now().Add(2 * kubeGrace) }
	if err := repo.commit(ctx, commit{project: "proj", state: &st}); err != nil {
		t.Fatalf("stray own claim = %v", err)
	}
}
