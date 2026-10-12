package network

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

func nadGet(c client.Client, name string) (*unstructured.Unstructured, error) {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvkNAD)
	err := c.Get(context.Background(), client.ObjectKey{Namespace: "vws-system", Name: name}, o)
	return o, err
}

func TestKubeOVNSubnetOwnsOneNAD(t *testing.T) {
	ctx := context.Background()
	k, c := newKube()
	sub := domain.Subnet{ID: "subnet-1", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")}
	for range 2 {
		if err := k.ensureSubnet(ctx, sub); err != nil {
			t.Fatal(err)
		}
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvkNAD.GroupVersion().WithKind("NetworkAttachmentDefinitionList"))
	if err := c.List(ctx, list); err != nil || len(list.Items) != 1 {
		t.Fatalf("nads %d err %v", len(list.Items), err)
	}
	o, err := nadGet(c, "subnet-1")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, _ := unstructured.NestedString(o.Object, "spec", "config")
	want := `{"cniVersion":"0.3.1","provider":"subnet-1.vws-system.ovn","server_socket":"/run/openvswitch/kube-ovn-daemon.sock","type":"kube-ovn"}`
	if cfg != want {
		t.Fatalf("config %s, want %s", cfg, want)
	}
	for range 2 {
		if err := k.deleteSubnet(ctx, sub); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := nadGet(c, "subnet-1"); !apierrors.IsNotFound(err) {
		t.Fatalf("nad still present: %v", err)
	}
}

func TestKubeOVNSubnetNeedsNADNamespace(t *testing.T) {
	k, c := newKube()
	k.nadNamespace = ""
	err := k.ensureSubnet(context.Background(), domain.Subnet{ID: "subnet-1", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")})
	if !isInvalid(err) {
		t.Fatalf("err %v, want Invalid", err)
	}
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvkSubnet)
	if err := c.Get(context.Background(), client.ObjectKey{Name: "subnet-1"}, o); !apierrors.IsNotFound(err) {
		t.Fatalf("subnet written without a NAD namespace: %v", err)
	}
}
