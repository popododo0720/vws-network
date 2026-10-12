package network

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

func isConflict(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Kind == domain.Conflict
}

func seed(t *testing.T, c client.Client, gvk schema.GroupVersionKind, ns, name, id string, spec map[string]any, mutate func(*unstructured.Unstructured)) {
	t.Helper()
	o := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	o.SetGroupVersionKind(gvk)
	o.SetNamespace(ns)
	o.SetName(name)
	if id != "" {
		o.SetAnnotations(map[string]string{annotationID: id})
	}
	if mutate != nil {
		mutate(o)
	}
	if err := c.Create(context.Background(), o); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureRejectsForeignObject(t *testing.T) {
	ctx := context.Background()
	sub := domain.Subnet{ID: "subnet-1", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")}
	vpc := domain.VPC{ID: "vpc-1", CIDR: mustP("10.0.0.0/16")}
	eip := domain.ElasticIP{ID: "eipalloc-1", Address: mustP("198.51.100.7/32").Addr()}
	subnetSpec := map[string]any{"vpc": "vpc-1", "cidrBlock": "10.0.1.0/24", "protocol": "IPv4", "provider": "subnet-1.vws-system.ovn"}
	for name, tc := range map[string]struct {
		seed func(*testing.T, client.Client)
		run  func(*kubeOVN) error
	}{
		"vpc without annotation": {
			func(t *testing.T, c client.Client) { seed(t, c, gvkVpc, "", "vpc-1", "", map[string]any{}, nil) },
			func(k *kubeOVN) error { return k.ensureVPC(ctx, vpc) },
		},
		"vpc with another id": {
			func(t *testing.T, c client.Client) { seed(t, c, gvkVpc, "", "vpc-1", "vpc-2", map[string]any{}, nil) },
			func(k *kubeOVN) error { return k.ensureVPC(ctx, vpc) },
		},
		"subnet with another cidr": {
			func(t *testing.T, c client.Client) {
				other := map[string]any{"vpc": "vpc-1", "cidrBlock": "10.0.2.0/24", "protocol": "IPv4", "provider": "subnet-1.vws-system.ovn"}
				seed(t, c, gvkSubnet, "", "subnet-1", "subnet-1", other, nil)
			},
			func(k *kubeOVN) error { return k.ensureSubnet(ctx, sub) },
		},
		"nad with another id": {
			func(t *testing.T, c client.Client) {
				seed(t, c, gvkNAD, "vws-system", "subnet-1", "subnet-2", map[string]any{"config": "{}"}, nil)
			},
			func(k *kubeOVN) error { return k.ensureNAD(ctx, "subnet-1") },
		},
		"nad with another config": {
			func(t *testing.T, c client.Client) {
				seed(t, c, gvkNAD, "vws-system", "subnet-1", "subnet-1", map[string]any{"config": "{}"}, nil)
			},
			func(k *kubeOVN) error { return k.ensureNAD(ctx, "subnet-1") },
		},
		"eip with another address": {
			func(t *testing.T, c client.Client) {
				seed(t, c, gvkEip, "", "eipalloc-1", "eipalloc-1", map[string]any{"externalSubnet": "external", "v4ip": "198.51.100.8", "type": "nat"}, nil)
			},
			func(k *kubeOVN) error { return k.ensureElasticIP(ctx, eip) },
		},
		"eip with another external subnet": {
			func(t *testing.T, c client.Client) {
				seed(t, c, gvkEip, "", "eipalloc-1", "eipalloc-1", map[string]any{"externalSubnet": "other", "v4ip": "198.51.100.7", "type": "nat"}, nil)
			},
			func(k *kubeOVN) error { return k.ensureElasticIP(ctx, eip) },
		},
	} {
		k, c := newKube()
		tc.seed(t, c)
		if err := tc.run(k); !isConflict(err) {
			t.Errorf("%s: err %v, want Conflict", name, err)
		}
	}
	k, c := newKube()
	seed(t, c, gvkSubnet, "", "subnet-1", "subnet-1", subnetSpec, nil)
	if err := k.ensureNAD(ctx, "subnet-1"); err != nil {
		t.Fatal(err)
	}
	if err := k.ensureSubnet(ctx, sub); err != nil {
		t.Fatalf("matching existing subnet: %v", err)
	}
}

func TestEnsureKeepsExistingObjectUntouched(t *testing.T) {
	ctx := context.Background()
	k, c := newKube()
	seed(t, c, gvkVpc, "", "vpc-1", "vpc-2", map[string]any{"marker": "theirs"}, nil)
	if err := k.ensureVPC(ctx, domain.VPC{ID: "vpc-1", CIDR: mustP("10.0.0.0/16")}); !isConflict(err) {
		t.Fatalf("err %v", err)
	}
	o := getObj(t, c, gvkVpc, "vpc-1")
	if m, _, _ := unstructured.NestedString(o.Object, "spec", "marker"); m != "theirs" || o.GetAnnotations()[annotationID] != "vpc-2" {
		t.Fatalf("object was rewritten: %v", o.Object)
	}
}

func TestEnsureTerminatingObjectIsRetryable(t *testing.T) {
	ctx := context.Background()
	k, c := newKube()
	seed(t, c, gvkVpc, "", "vpc-1", "vpc-1", map[string]any{}, func(o *unstructured.Unstructured) {
		o.SetFinalizers([]string{"vws.test/hold"})
	})
	if err := k.deleteVPC(ctx, domain.VPC{ID: "vpc-1"}); err != nil {
		t.Fatal(err)
	}
	if getObj(t, c, gvkVpc, "vpc-1").GetDeletionTimestamp() == nil {
		t.Fatal("object is not terminating")
	}
	err := k.ensureVPC(ctx, domain.VPC{ID: "vpc-1", CIDR: mustP("10.0.0.0/16")})
	if err == nil || isConflict(err) || isInvalid(err) {
		t.Fatalf("err %v, want a plain retryable error", err)
	}
}

func TestEnsureGroupIgnoresRuleLists(t *testing.T) {
	ctx := context.Background()
	k, _ := newKube()
	g := domain.SecurityGroup{ID: "sg-1"}
	if err := k.ensureGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	r := domain.Rule{Direction: domain.Ingress, Protocol: domain.All, FromPort: -1, ToPort: -1, Peer: domain.Peer{CIDR: mustP("0.0.0.0/0")}}
	if err := k.ensureRule(ctx, "sg-1", r); err != nil {
		t.Fatal(err)
	}
	if err := k.ensureGroup(ctx, g); err != nil {
		t.Fatalf("group with rules: %v", err)
	}
}

func TestEnsureGatewayIgnoresHostSubnet(t *testing.T) {
	ctx := context.Background()
	k, _ := newKube()
	k.snat = true
	for _, s := range []domain.Subnet{
		{ID: "subnet-1", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")},
		{ID: "subnet-2", VPC: "vpc-1", CIDR: mustP("10.0.2.0/24")},
	} {
		if err := k.ensureSubnet(ctx, s); err != nil {
			t.Fatalf("%s: %v", s.ID, err)
		}
	}
}
