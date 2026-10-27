package network

import (
	"context"
	"crypto/sha256"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/popododo0720/vws-network/internal/domain"
)

const (
	upgradeFixtureProject = domain.ProjectID("a1001")

	upgradeFingerprintSucceeded = "upgrade-fixture-succeeded"
	upgradeFingerprintFailed    = "upgrade-fixture-failed"
	upgradeFingerprintPending   = "upgrade-fixture-pending"
)

var upgradeClaimAt = time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)

// TestUpgradeFixtures opens every released build under testdata/upgrade and
// reads it with the current code. The database must reach the current schema,
// every stored field must decode, an idempotency key must replay, and the
// Kube-OVN objects the build wrote must pass the current ensure.
func TestUpgradeFixtures(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("testdata", "upgrade", "*"))
	if err != nil {
		t.Fatal(err)
	}
	fixtures := 0
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			continue
		}
		fixtures++
		t.Run(filepath.Base(dir), func(t *testing.T) { testUpgradeFixture(t, dir) })
	}
	if fixtures == 0 {
		t.Fatal("no upgrade fixture directory")
	}
}

func testUpgradeFixture(t *testing.T, dir string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "net.db")
	data, err := os.ReadFile(filepath.Join(dir, "net.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	repo, err := openSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeOrFail(t, repo.close) })

	var version int
	if err := repo.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("user_version = %d, want %d", version, schemaVersion)
	}
	ids, err := repo.projectIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []domain.ProjectID{upgradeFixtureProject}) {
		t.Fatalf("projects = %v", ids)
	}
	st, ver, err := repo.load(ctx, upgradeFixtureProject)
	if err != nil {
		t.Fatal(err)
	}
	if ver != 1 {
		t.Fatalf("project version = %d, want 1", ver)
	}
	checkUpgradeState(t, st)
	checkUpgradeOperations(t, ctx, repo)
	checkUpgradeAddresses(t, ctx, repo)
	checkUpgradeObjectsAccepted(t, ctx, st, readUpgradeObjects(t, dir))
}

// checkUpgradeState reads back every field of the project document, including
// the revoking rule and the detaching address that have no getter.
func checkUpgradeState(t *testing.T, st domain.State) {
	t.Helper()
	vpc, ok := st.VPCs["vpc-1"]
	if !ok {
		t.Fatal("vpc-1 is missing")
	}
	wantVPC := domain.VPC{
		ID: "vpc-1", CIDR: mustP("10.8.0.0/16"), DefaultGroup: "sg-1", Region: "r1",
		Description: "upgrade fixture vpc", Labels: map[string]string{"env": "upgrade"}, Status: domain.StatusAvailable,
	}
	if !reflect.DeepEqual(vpc, wantVPC) {
		t.Fatalf("vpc = %+v", vpc)
	}
	sub, ok := st.Subnets["subnet-1"]
	if !ok {
		t.Fatal("subnet-1 is missing")
	}
	wantSubnet := domain.Subnet{
		ID: "subnet-1", VPC: "vpc-1", CIDR: mustP("10.8.1.0/24"), Zone: "az1", Region: "r1",
		Description: "upgrade fixture subnet", Labels: map[string]string{"tier": "app"}, Status: domain.StatusAvailable,
	}
	if !reflect.DeepEqual(sub, wantSubnet) {
		t.Fatalf("subnet = %+v", sub)
	}
	def, ok := st.Groups["sg-1"]
	if !ok {
		t.Fatal("the default group is missing")
	}
	if def.VPC != "vpc-1" || def.Name != "default" || !def.Default || def.Region != "r1" ||
		def.Status != domain.StatusAvailable {
		t.Fatalf("default group = %+v", def)
	}
	if !reflect.DeepEqual(def.Rules, []domain.Rule{domain.DefaultEgress()}) {
		t.Fatalf("default group rules = %+v", def.Rules)
	}
	group, ok := st.Groups["sg-2"]
	if !ok {
		t.Fatal("sg-2 is missing")
	}
	if group.VPC != "vpc-1" || group.Name != "web" || group.Default || group.Region != "r1" ||
		group.Description != "upgrade fixture group" || !reflect.DeepEqual(group.Labels, map[string]string{"role": "web"}) ||
		group.Status != domain.StatusAvailable {
		t.Fatalf("group = %+v", group)
	}
	ingress := domain.Rule{
		Direction: domain.Ingress, Protocol: domain.TCP, FromPort: 80, ToPort: 80,
		Peer: domain.Peer{CIDR: mustP("0.0.0.0/0")},
	}
	peer := domain.Rule{
		Direction: domain.Egress, Protocol: domain.All, FromPort: -1, ToPort: -1,
		Peer: domain.Peer{Group: "sg-1"},
	}
	wantRules, err := domain.CanonicalRules([]domain.Rule{domain.DefaultEgress(), ingress, peer})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(group.Rules, wantRules) {
		t.Fatalf("group rules = %+v, want %+v", group.Rules, wantRules)
	}
	dns := domain.Rule{
		Direction: domain.Ingress, Protocol: domain.UDP, FromPort: 53, ToPort: 53,
		Peer: domain.Peer{CIDR: mustP("10.8.1.0/24")},
	}
	actions, ok, err := st.Plan("sg-2")
	if err != nil || !ok {
		t.Fatalf("plan sg-2: ok %v err %v", ok, err)
	}
	if !slices.ContainsFunc(actions, func(a domain.Action) bool {
		return a.Kind == domain.ActDeleteRule && a.Rule == dns
	}) {
		t.Fatalf("revoking rule is not planned for delete: %+v", actions)
	}
	eip, ok := st.ElasticIPs["eipalloc-1"]
	if !ok {
		t.Fatal("eipalloc-1 is missing")
	}
	wantEIP := domain.ElasticIP{
		ID: "eipalloc-1", VPC: "vpc-1", Address: netip.MustParseAddr("198.51.100.1"),
		Private: netip.MustParseAddr("10.8.1.11"), Subnet: "subnet-1",
		Instance: "i-0123456789abcdef0", Device: "eth0", Region: "r1",
		Labels: map[string]string{"app": "web"}, Status: domain.StatusAvailable,
	}
	// detaching is unexported, so compare the stored fields one by one and
	// check the detaching address through Plan below.
	if eip.ID != wantEIP.ID || eip.VPC != wantEIP.VPC || eip.Address != wantEIP.Address ||
		eip.Private != wantEIP.Private || eip.Subnet != wantEIP.Subnet || eip.Instance != wantEIP.Instance ||
		eip.Device != wantEIP.Device || eip.Region != wantEIP.Region ||
		!reflect.DeepEqual(eip.Labels, wantEIP.Labels) || eip.Status != wantEIP.Status {
		t.Fatalf("elastic ip = %+v", eip)
	}
	actions, ok, err = st.Plan("eipalloc-1")
	if err != nil || !ok {
		t.Fatalf("plan eipalloc-1: ok %v err %v", ok, err)
	}
	if !slices.ContainsFunc(actions, func(a domain.Action) bool {
		return a.Kind == domain.ActDeleteNAT && a.Addr == netip.MustParseAddr("10.8.1.10")
	}) {
		t.Fatalf("detaching address is not planned for delete: %+v", actions)
	}
}

// checkUpgradeOperations reads every operation column and replays each
// idempotency key through the lookup the service uses.
func checkUpgradeOperations(t *testing.T, ctx context.Context, repo *sqliteRepo) {
	t.Helper()
	cases := []struct {
		key       domain.IdempotencyKey
		id        string
		typ       domain.OperationType
		resource  string
		state     domain.OpState
		attempts  int
		notBefore time.Time
		token     string
		errKind   domain.ErrKind
		errMsg    string
		seed      string
	}{
		{
			key: "key-succeeded", id: "op-succeeded", typ: domain.OpAuthorizeRules, resource: "sg-2",
			state: domain.OpSucceeded, attempts: 1, notBefore: upgradeClaimAt.Add(-2 * time.Hour),
			token: "claim-op-succeeded", seed: upgradeFingerprintSucceeded,
		},
		{
			key: "key-failed", id: "op-failed", typ: domain.OpCreateSubnet, resource: "subnet-1",
			state: domain.OpFailed, attempts: 3, notBefore: upgradeClaimAt.Add(-time.Hour),
			token: "claim-op-failed", errKind: domain.Conflict, errMsg: "subnet subnet-1 has a different cidr",
			seed: upgradeFingerprintFailed,
		},
		{
			key: "key-pending", id: "op-pending", typ: domain.OpAllocateElasticIP, resource: "eipalloc-1",
			state: domain.OpPending, attempts: 0, notBefore: upgradeClaimAt.Add(time.Hour),
			seed: upgradeFingerprintPending,
		},
	}
	for _, tc := range cases {
		j, ok, err := repo.key(ctx, upgradeFixtureProject, tc.key)
		if err != nil || !ok {
			t.Fatalf("replay %s: ok %v err %v", tc.key, ok, err)
		}
		if j.op.ID != tc.id || j.op.Type != tc.typ || j.op.Resource != tc.resource || j.op.State != tc.state {
			t.Fatalf("replay %s = %+v", tc.key, j.op)
		}
		if j.op.Project != upgradeFixtureProject || j.op.Key != tc.key {
			t.Fatalf("replay %s project/key = %q/%q", tc.key, j.op.Project, j.op.Key)
		}
		if j.fingerprint != sha256.Sum256([]byte(tc.seed)) {
			t.Fatalf("replay %s fingerprint = %x", tc.key, j.fingerprint)
		}
		if j.lane != "vpc-1" || j.attempts != tc.attempts || !j.notBefore.Equal(tc.notBefore) || j.token != tc.token {
			t.Fatalf("replay %s job = %+v", tc.key, j)
		}
		if tc.errKind == 0 {
			if j.op.Err != nil {
				t.Fatalf("replay %s error = %+v", tc.key, j.op.Err)
			}
		} else if j.op.Err == nil || j.op.Err.Kind != tc.errKind || j.op.Err.Msg != tc.errMsg {
			t.Fatalf("replay %s error = %+v", tc.key, j.op.Err)
		}
		op, err := repo.operation(ctx, upgradeFixtureProject, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(op, j.op) {
			t.Fatalf("operation %s = %+v, want %+v", tc.id, op, j.op)
		}
	}
	if pending, err := repo.pending(ctx); err != nil || pending != 1 {
		t.Fatalf("pending = %d, err %v", pending, err)
	}
}

func checkUpgradeAddresses(t *testing.T, ctx context.Context, repo *sqliteRepo) {
	t.Helper()
	inUse, err := repo.addressesInUse(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[netip.Addr]bool{netip.MustParseAddr("198.51.100.1"): true}
	if !reflect.DeepEqual(inUse, want) {
		t.Fatalf("addresses in use = %v", inUse)
	}
}

// readUpgradeObjects decodes every captured object and checks the id
// annotation that ties it to its resource.
func readUpgradeObjects(t *testing.T, dir string) []*unstructured.Unstructured {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(paths)
	var out []*unstructured.Unstructured
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var obj map[string]any
		if err := utilyaml.Unmarshal(data, &obj); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		o := &unstructured.Unstructured{Object: obj}
		if o.GetAPIVersion() == "" || o.GetKind() == "" || o.GetName() == "" {
			t.Fatalf("%s: incomplete object %v", path, obj)
		}
		if got := o.GetAnnotations()[annotationID]; got != o.GetName() {
			t.Fatalf("%s: id annotation = %q, want %q", path, got, o.GetName())
		}
		out = append(out, o)
	}
	if len(out) == 0 {
		t.Fatalf("%s: no captured objects", dir)
	}
	return out
}

// checkUpgradeObjectsAccepted runs the current ensure against the captured
// objects. Create reports AlreadyExists, and ensure then compares the id
// annotation and every spec field this build writes.
func checkUpgradeObjectsAccepted(
	t *testing.T, ctx context.Context, st domain.State, objs []*unstructured.Unstructured,
) {
	t.Helper()
	existing := make([]client.Object, 0, len(objs)+1)
	existing = append(existing, externalSubnetObject("external", "10.78.0.1"))
	for _, o := range objs {
		existing = append(existing, o)
	}
	k := &kubeOVN{
		c: fake.NewClientBuilder().WithObjects(existing...).Build(), externalSubnet: "external",
		nadNamespace: "vws-system", snat: true, vmMTU: DefaultVMMTU,
	}
	if err := k.ensureVPC(ctx, st.VPCs["vpc-1"]); err != nil {
		t.Fatalf("vpc: %v", err)
	}
	for _, id := range []domain.SecurityGroupID{"sg-1", "sg-2"} {
		g := st.Groups[id]
		if err := k.ensureGroup(ctx, g); err != nil {
			t.Fatalf("group %s: %v", id, err)
		}
		for _, r := range g.Rules {
			if err := k.ensureRule(ctx, g.ID, r); err != nil {
				t.Fatalf("rule %s %+v: %v", id, r, err)
			}
		}
	}
	if err := k.ensureSubnet(ctx, st.Subnets["subnet-1"]); err != nil {
		t.Fatalf("subnet: %v", err)
	}
	eip := st.ElasticIPs["eipalloc-1"]
	if err := k.ensureElasticIP(ctx, eip); err != nil {
		t.Fatalf("elastic ip: %v", err)
	}
	if err := k.ensureNAT(ctx, eip, eip.Private); err != nil {
		t.Fatalf("nat: %v", err)
	}
}
