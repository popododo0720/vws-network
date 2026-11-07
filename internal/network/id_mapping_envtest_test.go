//go:build envtest

package network

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

// TestEnvtestAWSIDMapsToObjects opens the service on the Kubernetes store and
// the Kube-OVN dataplane. For each resource kind the store document is found
// by its id label and the Kube-OVN object by its id annotation, and each
// object gives back the id. A second project cannot take the same id.
func TestEnvtestAWSIDMapsToObjects(t *testing.T) {
	env, admin := startEnv(t)
	ctx := context.Background()
	kc := kubeconfigFor(t, env)
	svc, err := Open(ctx, Config{
		Store:       StoreConfig{Namespace: envNamespace, Kubeconfig: kc},
		AddressPool: envPool,
		Workers:     1,
		KubeOVN:     KubeOVNConfig{Kubeconfig: kc, ExternalSubnet: "external", NADNamespace: envNamespace},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, svc.Close)
	vpcOp, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.8.0.0/16"), ID: "app"})
	if err != nil {
		t.Fatal(err)
	}
	sub := domain.CreateSubnet{VPC: "app", CIDR: mustP("10.8.1.0/24"), Zone: "az1"}
	subOp, err := svc.CreateSubnet(ctx, call("proj", "sub"), sub)
	if err != nil {
		t.Fatal(err)
	}
	sgOp, err := svc.CreateSecurityGroup(ctx, call("proj", "sg"), domain.CreateSecurityGroup{VPC: "app", Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	eipOp, err := svc.AllocateElasticIP(ctx, call("proj", "eip"), domain.AllocateElasticIP{VPC: "app"})
	if err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, svc, "proj", vpcOp, subOp, sgOp, eipOp)

	for _, c := range []struct {
		kind string
		id   string
		gvk  schema.GroupVersionKind
	}{
		{kindVPC, vpcOp.Resource, gvkVpc},
		{kindSubnet, subOp.Resource, gvkSubnet},
		{kindGroup, sgOp.Resource, gvkGroup},
		{kindEIP, eipOp.Resource, gvkEip},
	} {
		var docs corev1.ConfigMapList
		if err := admin.List(ctx, &docs, client.InNamespace(envNamespace),
			client.MatchingLabels{labelStore: storeResource, labelKind: c.kind, labelID: c.id}); err != nil {
			t.Fatal(err)
		}
		if len(docs.Items) != 1 || docs.Items[0].Data["id"] != c.id || docs.Items[0].Data["kind"] != c.kind {
			t.Fatalf("%s %s: store documents by id label = %d", c.kind, c.id, len(docs.Items))
		}
		var objs unstructured.UnstructuredList
		objs.SetGroupVersionKind(c.gvk.GroupVersion().WithKind(c.gvk.Kind + "List"))
		if err := admin.List(ctx, &objs); err != nil {
			t.Fatal(err)
		}
		var named []string
		for _, o := range objs.Items {
			if o.GetAnnotations()[annotationID] == c.id {
				named = append(named, o.GetName())
			}
		}
		if len(named) != 1 || named[0] != c.id {
			t.Fatalf("%s %s: %s objects by id annotation = %v", c.kind, c.id, c.gvk.Kind, named)
		}
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(c.gvk)
		if err := admin.Get(ctx, client.ObjectKey{Name: c.id}, o); err != nil {
			t.Fatal(err)
		}
		if got := o.GetAnnotations()[annotationID]; got != c.id {
			t.Fatalf("%s %s: id from object = %q", c.kind, c.id, got)
		}
	}

	dup := domain.CreateVPC{CIDR: mustP("10.9.0.0/16"), ID: "app"}
	if _, err := svc.CreateVPC(ctx, call("other", "vpc"), dup); kind(err) != domain.Conflict {
		t.Fatalf("vpc id of another project = %v", err)
	}
	if vpcs, err := svc.ListVPCs(ctx, "other"); err != nil || len(vpcs) != 0 {
		t.Fatalf("refused create stored %v err %v", vpcs, err)
	}
}
