package network

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/popododo0720/vws-network/internal/domain"
)

func newKube() (*kubeOVN, client.Client) {
	c := fake.NewClientBuilder().Build()
	return &kubeOVN{c: c}, c
}

func isInvalid(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Kind == domain.Invalid
}

func getObj(t *testing.T, c client.Client, gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	t.Helper()
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvk)
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, o); err != nil {
		t.Fatalf("get %s/%s: %v", gvk.Kind, name, err)
	}
	return o
}

func TestKubeOVNVPCAndSubnet(t *testing.T) {
	ctx := context.Background()
	k, c := newKube()
	vpc := domain.VPC{ID: "vpc-1", CIDR: mustP("10.0.0.0/16")}
	sub := domain.Subnet{ID: "subnet-1", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")}
	for range 2 {
		if err := k.ensureVPC(ctx, vpc); err != nil {
			t.Fatal(err)
		}
		if err := k.ensureSubnet(ctx, sub); err != nil {
			t.Fatal(err)
		}
	}
	v := getObj(t, c, gvkVpc, "vpc-1")
	if v.GetAnnotations()[annotationID] != "vpc-1" {
		t.Fatalf("vpc annotations %v", v.GetAnnotations())
	}
	s := getObj(t, c, gvkSubnet, "subnet-1")
	spec, _, _ := unstructured.NestedStringMap(s.Object, "spec")
	want := map[string]string{"vpc": "vpc-1", "cidrBlock": "10.0.1.0/24", "protocol": "IPv4"}
	for key, w := range want {
		if spec[key] != w {
			t.Fatalf("subnet spec[%s] = %q, want %q", key, spec[key], w)
		}
	}
	if s.GetAnnotations()[annotationID] != "subnet-1" {
		t.Fatalf("subnet annotations %v", s.GetAnnotations())
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: "SubnetList"})
	if err := c.List(ctx, list); err != nil || len(list.Items) != 1 {
		t.Fatalf("subnets %d err %v", len(list.Items), err)
	}
}

func TestKubeOVNDeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	k, c := newKube()
	if err := k.deleteSubnet(ctx, domain.Subnet{ID: "subnet-9"}); err != nil {
		t.Fatalf("delete absent subnet: %v", err)
	}
	if err := k.deleteVPC(ctx, domain.VPC{ID: "vpc-9"}); err != nil {
		t.Fatalf("delete absent vpc: %v", err)
	}
	if err := k.ensureVPC(ctx, domain.VPC{ID: "vpc-1", CIDR: mustP("10.0.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := k.deleteVPC(ctx, domain.VPC{ID: "vpc-1"}); err != nil {
			t.Fatal(err)
		}
	}
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvkVpc)
	if err := c.Get(ctx, client.ObjectKey{Name: "vpc-1"}, o); !apierrors.IsNotFound(err) {
		t.Fatalf("vpc still present: %v", err)
	}
}

func TestKubeOVNRejectsBadCIDR(t *testing.T) {
	ctx := context.Background()
	k, c := newKube()
	err := k.ensureVPC(ctx, domain.VPC{ID: "vpc-1", CIDR: mustP("0.0.0.0/8")})
	if !isInvalid(err) {
		t.Fatalf("vpc err %v", err)
	}
	err = k.ensureSubnet(ctx, domain.Subnet{ID: "subnet-1", VPC: "vpc-1", CIDR: mustP("10.0.0.0/29")})
	if !isInvalid(err) {
		t.Fatalf("subnet err %v", err)
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: "VpcList"})
	if err := c.List(ctx, list); err != nil || len(list.Items) != 0 {
		t.Fatalf("rejected vpc wrote %d objects, err %v", len(list.Items), err)
	}
}
