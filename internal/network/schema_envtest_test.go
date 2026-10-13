//go:build envtest

package network

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

// strictClient fails the test when the API server drops or changes a spec
// field this repo wrote. The CRDs in testdata are the real Kube-OVN schemas,
// which prune unknown fields, so a misspelled field (v4ip for v4Ip) shows up
// as a field missing from what the server stored.
type strictClient struct {
	client.Client
	t *testing.T
}

func (s strictClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	sent := sentSpec(obj)
	if err := s.Client.Create(ctx, obj, opts...); err != nil {
		return err
	}
	s.check("create", obj, sent)
	return nil
}

func (s strictClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	sent := sentSpec(obj)
	if err := s.Client.Update(ctx, obj, opts...); err != nil {
		return err
	}
	s.check("update", obj, sent)
	return nil
}

func sentSpec(obj client.Object) any {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	return normalizeJSON(u.Object["spec"])
}

func (s strictClient) check(verb string, obj client.Object, sent any) {
	s.t.Helper()
	u, ok := obj.(*unstructured.Unstructured)
	if !ok || sent == nil {
		return
	}
	if err := keptIn(sent, normalizeJSON(u.Object["spec"]), "spec"); err != nil {
		s.t.Errorf("%s %s %s: the API server did not keep what we wrote: %v", verb, u.GetKind(), u.GetName(), err)
	}
}

func normalizeJSON(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}

// keptIn reports the first field of sent that stored lacks or holds with
// another value. Fields the server added are fine: they are defaults.
func keptIn(sent, stored any, path string) error {
	sm, ok := sent.(map[string]any)
	if !ok {
		if !reflect.DeepEqual(sent, stored) {
			return fmt.Errorf("%s = %v, sent %v", path, stored, sent)
		}
		return nil
	}
	gm, ok := stored.(map[string]any)
	if !ok {
		return fmt.Errorf("%s is %T, sent an object", path, stored)
	}
	for k, v := range sm {
		g, ok := gm[k]
		if !ok {
			return fmt.Errorf("%s.%s was pruned (not in the CRD schema)", path, k)
		}
		if err := keptIn(v, g, path+"."+k); err != nil {
			return err
		}
	}
	return nil
}

func strictKube(t *testing.T, c client.Client, snat bool) *kubeOVN {
	k := envKube(c, snat)
	k.c = strictClient{Client: c, t: t}
	return k
}

func TestStrictClientCatchesAPrunedField(t *testing.T) {
	_, c := startEnv(t)
	inner := &testing.T{}
	sc := strictClient{Client: c, t: inner}
	o := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"cidrBlock": "10.9.0.0/24", "protocol": "IPv4", "cidrblock": "typo"}}}
	o.SetGroupVersionKind(gvkSubnet)
	o.SetName("typo")
	if err := sc.Create(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !inner.Failed() {
		t.Fatal("a field the real Subnet schema prunes went unnoticed")
	}
}

// TestEnvtestSchemaKeepsEveryField writes every object the main paths make
// through the real Kube-OVN v1.16.12 schemas.
func TestEnvtestSchemaKeepsEveryField(t *testing.T) {
	_, c := startEnv(t)
	ctx := context.Background()
	k := strictKube(t, c, false)
	k.underlay = UnderlayConfig{ProviderNetwork: "vws-pn0", Interface: "ens3", VLANID: 100, CIDR: mustP("10.78.0.0/24"), Gateway: mustP("10.78.0.1/32").Addr()}
	if err := k.ensureUnderlay(ctx); err != nil {
		t.Fatal(err)
	}
	if err := k.ensureVPC(ctx, domain.VPC{ID: "vpc-1", CIDR: mustP("10.0.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	if err := k.ensureSubnet(ctx, domain.Subnet{ID: "subnet-1", VPC: "vpc-1", CIDR: mustP("10.0.1.0/24")}); err != nil {
		t.Fatal(err)
	}
	if err := k.ensureGroup(ctx, domain.SecurityGroup{ID: "sg-1"}); err != nil {
		t.Fatal(err)
	}
	rule := domain.Rule{Direction: domain.Ingress, Protocol: domain.TCP, FromPort: 22, ToPort: 22, Peer: domain.Peer{CIDR: mustP("0.0.0.0/0")}}
	if err := k.ensureRule(ctx, "sg-1", rule); err != nil {
		t.Fatal(err)
	}
}
