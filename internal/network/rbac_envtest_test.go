//go:build envtest

package network

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/popododo0720/vws-network/internal/domain"
)

// clusterRules is docs/rbac.md as code: every cluster scoped verb the
// Kube-OVN dataplane uses. nadRules is the NAD namespace Role.
var clusterRules = []rbacv1.PolicyRule{
	{APIGroups: []string{"kubeovn.io"}, Resources: []string{"vpcs", "ovn-eips", "ovn-snat-rules"}, Verbs: []string{"get", "create", "delete"}},
	{APIGroups: []string{"kubeovn.io"}, Resources: []string{"subnets"}, Verbs: []string{"get", "create", "delete"}},
	{APIGroups: []string{"kubeovn.io"}, Resources: []string{"security-groups"}, Verbs: []string{"get", "create", "update", "delete"}},
	{APIGroups: []string{"kubeovn.io"}, Resources: []string{"ovn-fips"}, Verbs: []string{"get", "list", "create", "delete"}},
	{APIGroups: []string{"kubeovn.io"}, Resources: []string{"provider-networks", "vlans"}, Verbs: []string{"get", "create"}},
	{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "patch"}},
}

var nadRules = []rbacv1.PolicyRule{
	{APIGroups: []string{"k8s.cni.cncf.io"}, Resources: []string{"network-attachment-definitions"}, Verbs: []string{"get", "create", "delete"}},
}

// userWith is a client for a user that holds only rules (and nadRules in the
// NAD namespace), the way the chart's ServiceAccount does.
func userWith(t *testing.T, env *envtest.Environment, admin client.Client, name string, rules []rbacv1.PolicyRule) client.Client {
	t.Helper()
	ctx := context.Background()
	subj := []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: name, APIGroup: rbacv1.GroupName}}
	for _, o := range []client.Object{
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: rules},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}, Subjects: subj, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: envNamespace}, Rules: nadRules},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: envNamespace}, Subjects: subj, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name}},
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
	return c
}

// TestEnvtestDocumentedRBACIsEnough runs every dataplane path as a user that
// holds exactly the documented rules.
func TestEnvtestDocumentedRBACIsEnough(t *testing.T) {
	env, admin := startEnv(t)
	ctx := context.Background()
	// The operator's external Subnet. startEnv may already make it.
	ext := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"cidrBlock": "10.78.0.0/24", "gateway": "10.78.0.1", "protocol": "IPv4"}}}
	ext.SetGroupVersionKind(gvkSubnet)
	ext.SetName("external")
	if err := admin.Create(ctx, ext); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	k := envKube(userWith(t, env, admin, "vws-network", clusterRules), true)
	vpc := domain.VPC{ID: "vpc-1", CIDR: mustP("10.0.0.0/16")}
	a := domain.Subnet{ID: "subnet-a", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")}
	b := domain.Subnet{ID: "subnet-b", VPC: "vpc-1", CIDR: mustP("10.0.2.0/24")}
	steps := []func() error{
		func() error { return k.ready(ctx) },
		func() error { return k.ensureVPC(ctx, vpc) },
		func() error { return k.ensureSubnet(ctx, a) },
		func() error { return k.ensureSubnet(ctx, b) },
		func() error { return k.ensureGroup(ctx, domain.SecurityGroup{ID: "sg-1"}) },
		func() error {
			return k.ensureRule(ctx, "sg-1", domain.Rule{Direction: domain.Ingress, Protocol: domain.TCP, FromPort: 22, ToPort: 22, Peer: domain.Peer{CIDR: mustP("0.0.0.0/0")}})
		},
		func() error { return k.deleteGroup(ctx, domain.SecurityGroup{ID: "sg-1"}) },
	}
	e := domain.ElasticIP{ID: "eipalloc-1", VPC: "vpc-1", Address: mustP("10.78.0.225/32").Addr()}
	private := mustP("10.0.1.5/32").Addr()
	steps = append(steps,
		func() error { return k.ensureElasticIP(ctx, e) },
		func() error { return k.ensureNAT(ctx, e, private) },
		func() error { return k.deleteNAT(ctx, e, private) },
		func() error { return k.deleteElasticIP(ctx, e) },
		// Deleting the subnet that hosts the VPC gateway lists subnets to
		// move the gateway (vws-test 45b, 45c).
		func() error { return k.deleteSubnet(ctx, a) },
		func() error { return k.deleteSubnet(ctx, b) },
		func() error { return k.deleteVPC(ctx, vpc) },
	)
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step %d as the documented role: %v", i, err)
		}
	}
	if err := admin.Create(ctx, node("gw-1", map[string]string{gatewayLabel: "false"})); err != nil {
		t.Fatal(err)
	}
	gw := envKube(k.c, false)
	gw.gatewayNodes = []string{"gw-1"}
	if _, err := gw.labelGatewayNodes(ctx); err != nil {
		t.Fatalf("gateway label as the documented role: %v", err)
	}
	if got, _ := nodeLabel(t, admin, "gw-1"); got != "true" {
		t.Fatalf("gateway label %q, want true", got)
	}
	u := envKube(k.c, false)
	u.externalSubnet = "vws-external"
	u.underlay = UnderlayConfig{ProviderNetwork: "vws-pn0", Interface: "ens3", VLANID: 100, CIDR: mustP("10.79.0.0/24"), Gateway: mustP("10.79.0.1/32").Addr()}
	if err := u.ensureUnderlay(ctx); err != nil {
		t.Fatalf("underlay as the documented role: %v", err)
	}
}

// TestEnvtestDeleteSubnetWithoutSubnetList is vws-test 45b after OVN-native
// SNAT: DeleteSubnet with -kube-snat no longer lists subnets to move a VPC
// gateway, so the documented role, which has no subnets list, deletes the
// subnet, its rule and its address.
func TestEnvtestDeleteSubnetWithoutSubnetList(t *testing.T) {
	env, admin := startEnv(t)
	ctx := context.Background()
	k := envKube(userWith(t, env, admin, "no-list", clusterRules), true)
	a := domain.Subnet{ID: "subnet-a", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")}
	if err := k.ensureSubnet(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := k.deleteSubnet(ctx, a); err != nil {
		t.Fatalf("delete with the documented role: %v", err)
	}
	if exists(admin, gvkSubnet, "", string(a.ID)) || exists(admin, gvkSnat, "", snatName(a.ID)) {
		t.Fatal("subnet or snat rule left behind")
	}
}
