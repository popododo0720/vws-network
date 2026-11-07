//go:build envtest

package network

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/popododo0720/vws-network/internal/domain"
)

// storeRules is the store Role of docs/rbac.md as code.
var storeRules = []rbacv1.PolicyRule{
	{
		APIGroups: []string{""},
		Resources: []string{"configmaps"},
		Verbs:     []string{"get", "list", "create", "update", "delete"},
	},
}

var envPool = mustP("10.78.0.128/25")

func openKubeStore(t *testing.T, kubeconfig string) *Service {
	t.Helper()
	svc, err := Open(context.Background(), Config{
		Store:       StoreConfig{Namespace: envNamespace, Kubeconfig: kubeconfig},
		AddressPool: envPool,
		Workers:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func waitSucceeded(t *testing.T, svc *Service, p domain.ProjectID, ops ...domain.Operation) {
	t.Helper()
	for _, op := range ops {
		waitFor(t, func() bool {
			got, err := svc.GetOperation(context.Background(), p, op.ID)
			return err == nil && got.State == domain.OpSucceeded
		})
	}
}

type described struct {
	VPCs    []domain.VPC
	Subnets []domain.Subnet
	Groups  []domain.SecurityGroup
	EIPs    []domain.ElasticIP
}

func describe(t *testing.T, svc *Service, p domain.ProjectID) described {
	t.Helper()
	ctx := context.Background()
	var d described
	var err error
	if d.VPCs, err = svc.ListVPCs(ctx, p); err != nil {
		t.Fatal(err)
	}
	if d.Subnets, err = svc.ListSubnets(ctx, p, ""); err != nil {
		t.Fatal(err)
	}
	if d.Groups, err = svc.ListSecurityGroups(ctx, p, ""); err != nil {
		t.Fatal(err)
	}
	if d.EIPs, err = svc.ListElasticIPs(ctx, p, ""); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestEnvtestStoreSurvivesRestart opens the service through Open on the
// Kubernetes store, closes it, and opens a new one on the same API server:
// every Describe is equal, and an operation a crash left RUNNING converges.
func TestEnvtestStoreSurvivesRestart(t *testing.T) {
	env, admin := startEnv(t)
	ctx := context.Background()
	kc := kubeconfigFor(t, env)
	first := openKubeStore(t, kc)
	vpcOp, err := first.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.8.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	vpc := domain.VPCID(vpcOp.Resource)
	subOp, err := first.CreateSubnet(
		ctx,
		call("proj", "sub"),
		domain.CreateSubnet{VPC: vpc, CIDR: mustP("10.8.1.0/24"), Zone: "az1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	rule := domain.Rule{
		Direction: domain.Ingress,
		Protocol:  domain.TCP,
		FromPort:  22,
		ToPort:    22,
		Peer:      domain.Peer{CIDR: mustP("0.0.0.0/0")},
	}
	sgOp, err := first.CreateSecurityGroup(
		ctx,
		call("proj", "sg"),
		domain.CreateSecurityGroup{VPC: vpc, Name: "web", Rules: []domain.Rule{rule}},
	)
	if err != nil {
		t.Fatal(err)
	}
	eipOp, err := first.AllocateElasticIP(ctx, call("proj", "eip"), domain.AllocateElasticIP{VPC: vpc})
	if err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, first, "proj", vpcOp, subOp, sgOp, eipOp)
	before := describe(t, first, "proj")
	if len(before.VPCs) != 1 || len(before.Subnets) != 1 || len(before.Groups) != 2 || len(before.EIPs) != 1 {
		t.Fatalf("before restart %+v", before)
	}
	closeOrFail(t, first.Close)

	second := openKubeStore(t, kc)
	if after := describe(t, second, "proj"); !reflect.DeepEqual(before, after) {
		t.Fatalf("after restart\n%+v\nwant\n%+v", after, before)
	}
	closeOrFail(t, second.Close)

	repo := openKubeRepo(t, admin, envNamespace)
	crashed, err := newService(ctx, repo, newFake(), envPool)
	if err != nil {
		t.Fatal(err)
	}
	op, err := crashed.CreateSecurityGroup(ctx, call("proj", "sg-2"), domain.CreateSecurityGroup{VPC: vpc, Name: "db"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repo.claim(ctx, crashed.now(), "crash"); err != nil || !ok {
		t.Fatal(err, ok)
	}
	if got, err := crashed.GetOperation(ctx, "proj", op.ID); err != nil || got.State != domain.OpRunning {
		t.Fatalf("claimed op %+v err %v", got, err)
	}

	third := openKubeStore(t, kc)
	defer closeOrFail(t, third.Close)
	waitSucceeded(t, third, "proj", op)
	m, ok := third.dp.(measured)
	if !ok {
		t.Fatalf("dataplane %T", third.dp)
	}
	if fake, ok := m.dataplane.(*fakeDataplane); !ok || !fake.has("sg/"+op.Resource) {
		t.Fatal("requeued operation did not reach the dataplane")
	}
	if after := describe(t, third, "proj"); len(after.Groups) != 3 || !reflect.DeepEqual(after.VPCs, before.VPCs) ||
		!reflect.DeepEqual(after.EIPs, before.EIPs) {
		t.Fatalf("after requeue %+v", after)
	}
}

func storeObjects(t *testing.T, c client.Client) map[string]int {
	t.Helper()
	var list corev1.ConfigMapList
	err := c.List(context.Background(), &list, client.InNamespace(envNamespace), client.HasLabels{labelStore})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, cm := range list.Items {
		k := cm.Labels[labelStore]
		if kind := cm.Data["kind"]; kind != "" {
			k += "/" + kind
		}
		out[k]++
	}
	return out
}

// TestEnvtestStoreReplayLeavesOneObject repeats a create with the same key:
// the same resource comes back and one object of each kind exists. A
// different body under that key is Conflict, as on every store, and writes
// nothing.
func TestEnvtestStoreReplayLeavesOneObject(t *testing.T) {
	env, admin := startEnv(t)
	ctx := context.Background()
	svc := openKubeStore(t, kubeconfigFor(t, env))
	defer closeOrFail(t, svc.Close)
	req := domain.CreateVPC{CIDR: mustP("10.4.0.0/16")}
	first, err := svc.CreateVPC(ctx, call("proj", "a"), req)
	if err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, svc, "proj", first)
	for range 3 {
		again, err := svc.CreateVPC(ctx, call("proj", "a"), req)
		if err != nil || again.ID != first.ID || again.Resource != first.Resource {
			t.Fatalf("replay %+v err %v, want %s", again, err, first.ID)
		}
	}
	want := map[string]int{
		storeHead:                1,
		storeKey:                 1,
		storeOp:                  1,
		storeID:                  2,
		storeResource + "/vpc":   1,
		storeResource + "/group": 1,
	}
	if got := storeObjects(t, admin); !reflect.DeepEqual(got, want) {
		t.Fatalf("store objects %v, want %v", got, want)
	}
	if _, err := svc.CreateVPC(ctx, call("proj", "a"), domain.CreateVPC{CIDR: mustP("10.5.0.0/16")}); kind(
		err,
	) != domain.Conflict {
		t.Fatalf("different body = %v, want Conflict", err)
	}
	if got := storeObjects(t, admin); !reflect.DeepEqual(got, want) {
		t.Fatalf("store objects after conflict %v, want %v", got, want)
	}
}

// TestEnvtestStoreRoleIsEnough runs the service on the Kubernetes store as a
// user that holds only the documented store Role in the store namespace.
func TestEnvtestStoreRoleIsEnough(t *testing.T) {
	env, admin := startEnv(t)
	ctx := context.Background()
	name := "vws-network-store"
	subj := []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: name, APIGroup: rbacv1.GroupName}}
	for _, o := range []client.Object{
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: envNamespace}, Rules: storeRules},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: envNamespace}, Subjects: subj,
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		},
	} {
		if err := admin.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	u, err := env.AddUser(envtest.User{Name: name}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(u.Config(), client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := newService(ctx, openKubeRepo(t, c, envNamespace), newFake(), envPool)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Ready(ctx); err != nil {
		t.Fatalf("ready as the store role: %v", err)
	}
	vpcOp, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.8.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	vpc := domain.VPCID(vpcOp.Resource)
	eipOp, err := svc.AllocateElasticIP(ctx, call("proj", "eip"), domain.AllocateElasticIP{VPC: vpc})
	if err != nil {
		t.Fatal(err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	for i, step := range []func() (domain.Operation, error){
		func() (domain.Operation, error) {
			eip := domain.ElasticIPID(eipOp.Resource)
			return svc.ReleaseElasticIP(ctx, call("proj", "rel"), domain.ReleaseElasticIP{ElasticIP: eip})
		},
		func() (domain.Operation, error) {
			return svc.DeleteVPC(ctx, call("proj", "del"), domain.DeleteVPC{VPC: vpc})
		},
	} {
		if _, err := step(); err != nil {
			t.Fatalf("step %d as the store role: %v", i, err)
		}
		if err := drain(svc); err != nil {
			t.Fatalf("step %d as the store role: %v", i, err)
		}
	}
	if vpcs, err := svc.ListVPCs(ctx, "proj"); err != nil || len(vpcs) != 0 {
		t.Fatalf("vpcs after delete %+v err %v", vpcs, err)
	}
	if got := storeObjects(t, admin)[storeAddress]; got != 0 {
		t.Fatalf("%d address claims left after release", got)
	}
	if got := storeObjects(t, admin)[storeID]; got != 0 {
		t.Fatalf("%d resource id claims left after delete", got)
	}
	later := &kubeRepo{c: c, ns: envNamespace, now: func() time.Time { return time.Now().Add(2 * kubeGrace) }}
	if err := later.repair(ctx); err != nil {
		t.Fatalf("repair as the store role: %v", err)
	}
	if got := storeObjects(t, admin); got[storeResource+"/vpc"] != 0 || got[storeResource+"/eip"] != 0 {
		t.Fatalf("tombstones left after repair past the grace period: %v", got)
	}
}

// TestEnvtestStoreRefusesMissingNamespace: a list in a missing namespace
// succeeds, so without the start probe the service would be ready and fail
// every write.
func TestEnvtestStoreRefusesMissingNamespace(t *testing.T) {
	env, _ := startEnv(t)
	_, err := Open(context.Background(), Config{
		Store:       StoreConfig{Namespace: "no-such-ns", Kubeconfig: kubeconfigFor(t, env)},
		AddressPool: envPool,
	})
	if err == nil || !strings.Contains(err.Error(), "store namespace no-such-ns") {
		t.Fatalf("missing namespace: err %v", err)
	}
}

// TestEnvtestStoreAcceptsWithoutClientThrottle: ten creates through Open are
// accepted in seconds. At the client-go default of 5 QPS they took about 7s
// each.
func TestEnvtestStoreAcceptsWithoutClientThrottle(t *testing.T) {
	env, _ := startEnv(t)
	ctx := context.Background()
	svc := openKubeStore(t, kubeconfigFor(t, env))
	defer closeOrFail(t, svc.Close)
	start := time.Now()
	for i := range 10 {
		p := fmt.Sprintf("proj-%d", i)
		if _, err := svc.CreateVPC(ctx, call(p, "vpc"), domain.CreateVPC{CIDR: mustP("10.8.0.0/16")}); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("10 creates accepted in %v, want under 10s", took)
	}
}
