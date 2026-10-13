package network

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

func exists(c client.Client, gvk schema.GroupVersionKind, ns, name string) bool {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvk)
	return c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, o) == nil
}

func TestKubeOVNRemoveRequiresOwnerAnnotation(t *testing.T) {
	cases := []struct {
		name     string
		id       string
		mutate   func(*unstructured.Unstructured)
		wantErr  bool
		wantKept bool
	}{
		{"owned", "vpc-1", nil, false, false},
		{"no annotation", "", nil, true, true},
		{"other annotation", "", func(o *unstructured.Unstructured) {
			o.SetAnnotations(map[string]string{"example.com/id": "vpc-1"})
		}, true, true},
		{"wrong id", "vpc-2", nil, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, c := newKube()
			seed(t, c, gvkVpc, "", "vpc-1", tc.id, map[string]any{}, tc.mutate)
			err := k.deleteVPC(t.Context(), domain.VPC{ID: "vpc-1"})
			if tc.wantErr != isConflict(err) || (!tc.wantErr && err != nil) {
				t.Fatalf("err = %v, want conflict %v", err, tc.wantErr)
			}
			if exists(c, gvkVpc, "", "vpc-1") != tc.wantKept {
				t.Fatalf("exists = %v, want %v", !tc.wantKept, tc.wantKept)
			}
		})
	}
}

func TestKubeOVNRemoveMissingSucceeds(t *testing.T) {
	k, _ := newKube()
	if err := k.deleteVPC(context.Background(), domain.VPC{ID: "vpc-1"}); err != nil {
		t.Fatal(err)
	}
}

func TestKubeOVNRemoveNADRequiresOwnerAnnotation(t *testing.T) {
	ctx := context.Background()
	k, c := newKube()
	seed(t, c, gvkNAD, "vws-system", "subnet-1", "", map[string]any{}, nil)
	if err := k.removeNAD(ctx, "subnet-1"); !isConflict(err) {
		t.Fatalf("err = %v, want conflict", err)
	}
	if !exists(c, gvkNAD, "vws-system", "subnet-1") {
		t.Fatal("foreign NAD was deleted")
	}
	seed(t, c, gvkNAD, "vws-system", "subnet-2", "subnet-2", map[string]any{}, nil)
	if err := k.removeNAD(ctx, "subnet-2"); err != nil {
		t.Fatal(err)
	}
	if exists(c, gvkNAD, "vws-system", "subnet-2") {
		t.Fatal("owned NAD was kept")
	}
}
