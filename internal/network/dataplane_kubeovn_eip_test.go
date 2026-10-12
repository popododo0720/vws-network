package network

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

func eipFixture() (domain.ElasticIP, string) {
	return domain.ElasticIP{ID: "eip-1", VPC: "vpc-1", Address: mustP("198.51.100.7/32").Addr()}, "10.0.1.5"
}

func TestKubeOVNElasticIPLifecycle(t *testing.T) {
	ctx := context.Background()
	k, c := newKube()
	e, private := eipFixture()
	addr := mustP(private + "/32").Addr()
	for range 2 {
		if err := k.ensureElasticIP(ctx, e); err != nil {
			t.Fatal(err)
		}
		if err := k.ensureNAT(ctx, e, addr); err != nil {
			t.Fatal(err)
		}
	}
	eip := getObj(t, c, gvkEip, "eip-1")
	spec := nestedStringMap(eip.Object, "spec")
	for key, w := range map[string]string{"externalSubnet": "external", "v4ip": "198.51.100.7", "type": "nat"} {
		if spec[key] != w {
			t.Fatalf("eip spec[%s] = %q, want %q", key, spec[key], w)
		}
	}
	fip := getObj(t, c, gvkFip, "eip-1-10-0-1-5")
	spec = nestedStringMap(fip.Object, "spec")
	for key, w := range map[string]string{"ovnEip": "eip-1", "vpc": "vpc-1", "v4Ip": "10.0.1.5"} {
		if spec[key] != w {
			t.Fatalf("fip spec[%s] = %q, want %q", key, spec[key], w)
		}
	}
	if err := k.deleteElasticIP(ctx, e); err == nil {
		t.Fatal("released an elastic ip that is still bound")
	}
	for range 2 {
		if err := k.deleteNAT(ctx, e, addr); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := k.deleteElasticIP(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvkEip)
	if err := c.Get(ctx, client.ObjectKey{Name: "eip-1"}, o); !apierrors.IsNotFound(err) {
		t.Fatalf("eip still present: %v", err)
	}
}

func TestKubeOVNElasticIPNeedsExternalSubnet(t *testing.T) {
	k, _ := newKube()
	k.externalSubnet = ""
	e, _ := eipFixture()
	if err := k.ensureElasticIP(context.Background(), e); !isInvalid(err) {
		t.Fatalf("err %v, want Invalid", err)
	}
}

func TestKubeOVNDeleteAbsentElasticIPAndNAT(t *testing.T) {
	ctx := context.Background()
	k, _ := newKube()
	e, private := eipFixture()
	if err := k.deleteNAT(ctx, e, mustP(private+"/32").Addr()); err != nil {
		t.Fatal(err)
	}
	if err := k.deleteElasticIP(ctx, e); err != nil {
		t.Fatal(err)
	}
}
