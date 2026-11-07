package network

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/popododo0720/vws-network/internal/domain"
)

// widestText is the largest description and label set a request may carry,
// in characters JSON escapes to six bytes each.
func widestText() domain.Text {
	labels := map[string]string{}
	for i := range 64 {
		labels[fmt.Sprintf("k%02d", i)+strings.Repeat("x", 60)] = strings.Repeat("<", 63)
	}
	return domain.Text{Description: strings.Repeat("<", 1024), Labels: labels}
}

// largestObjects fails t when a store object is over kubeObjectLimit and
// returns the largest size per store.
func largestObjects(t *testing.T, c client.Client) map[string]int {
	t.Helper()
	var list corev1.ConfigMapList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	most := map[string]int{}
	for i := range list.Items {
		cm := &list.Items[i]
		b, err := json.Marshal(cm)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > kubeObjectLimit {
			t.Fatalf("%s is %d bytes, over %d", cm.Name, len(b), kubeObjectLimit)
		}
		store := cm.Labels[labelStore]
		most[store] = max(most[store], len(b))
	}
	return most
}

func TestKubeRepoLargestProjectFitsLimit(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().Build()
	svc, _ := openTest(t, openKubeRepo(t, c, "vws-system"))
	n := 0
	most := map[string]int{}
	run := func(op domain.Operation, err error) domain.Operation {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := drain(svc); err != nil {
			t.Fatal(err)
		}
		for store, size := range largestObjects(t, c) {
			most[store] = max(most[store], size)
		}
		return op
	}
	key := func() domain.Call { n++; return call("proj", fmt.Sprintf("k%d", n)) }
	text := widestText()

	vpcOp := run(svc.CreateVPC(ctx, key(), domain.CreateVPC{CIDR: mustP("10.0.0.0/16"), Text: text}))
	vpcID := domain.VPCID(vpcOp.Resource)
	vpc, err := svc.GetVPC(ctx, "proj", vpcID)
	if err != nil {
		t.Fatal(err)
	}
	subOp := run(svc.CreateSubnet(ctx, key(),
		domain.CreateSubnet{VPC: vpcID, CIDR: mustP("10.0.1.0/24"), Zone: "az1", Text: text}))
	sgOp := run(svc.CreateSecurityGroup(ctx, key(),
		domain.CreateSecurityGroup{VPC: vpcID, Name: strings.Repeat("<", 255)}))
	eipOp := run(svc.AllocateElasticIP(ctx, key(), domain.AllocateElasticIP{VPC: vpcID, Labels: text.Labels}))

	// Rules are the one count without a bound of its own; the store bounds
	// it. Grow the default group, which a VPC delete changes together with
	// the VPC, until the store refuses.
	group := vpc.DefaultGroup
	port := int32(0)
	refused := false
	for range 200 {
		var rules []domain.Rule
		for range 100 {
			port++
			rules = append(rules, domain.Rule{
				Direction: domain.Ingress, Protocol: domain.TCP, FromPort: port, ToPort: port,
				Peer: domain.Peer{CIDR: mustP("192.168.0.0/16")},
			})
		}
		op, err := svc.AuthorizeRules(ctx, key(), domain.AuthorizeRules{Group: group, Rules: rules})
		if kind(err) == domain.Invalid {
			refused = true
			break
		}
		run(op, err)
	}
	if !refused {
		t.Fatal("the store never refused a growing group")
	}
	g, err := svc.GetSecurityGroup(ctx, "proj", group)
	if err != nil {
		t.Fatal(err)
	}
	stored := len(g.Rules)
	if stored != int(port)-100+1 {
		t.Fatalf("refused authorize left %d rules, want %d", stored, int(port)-100+1)
	}
	// AWS allows at most 1000 rules per group (60 inbound by default).
	if stored < 1000 {
		t.Fatalf("store holds only %d rules in one group", stored)
	}

	run(svc.RevokeRules(ctx, key(), domain.RevokeRules{Group: group, Rules: g.Rules[:10]}))
	run(svc.DeleteSubnet(ctx, key(), domain.DeleteSubnet{Subnet: domain.SubnetID(subOp.Resource)}))
	run(svc.DeleteSecurityGroup(ctx, key(), domain.DeleteSecurityGroup{Group: domain.SecurityGroupID(sgOp.Resource)}))
	run(svc.ReleaseElasticIP(ctx, key(), domain.ReleaseElasticIP{ElasticIP: domain.ElasticIPID(eipOp.Resource)}))
	run(svc.DeleteVPC(ctx, key(), domain.DeleteVPC{VPC: vpcID}))
	if _, err := svc.GetVPC(ctx, "proj", vpcID); kind(err) != domain.NotFound {
		t.Fatalf("vpc with the largest default group was not deleted: %v", err)
	}
	t.Logf("%d rules in one group; largest objects %v", stored, most)
}

func TestKubeRepoRefusesHeadTooLargeBeforeSwap(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().Build()
	repo := openKubeRepo(t, c, "vws-system")
	st := domain.State{Groups: map[domain.SecurityGroupID]domain.SecurityGroup{}}
	for i := range 4 {
		id := domain.SecurityGroupID(fmt.Sprintf("sg-%d", i))
		st.Groups[id] = domain.SecurityGroup{ID: id, VPC: "vpc-1", Name: strings.Repeat("g", 80<<10)}
	}
	err := repo.commit(ctx, commit{project: "proj", state: &st})
	if kind(err) != domain.Invalid {
		t.Fatalf("oversized change = %v", err)
	}
	var list corev1.ConfigMapList
	if err := c.List(ctx, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("refused change left %d objects", len(list.Items))
	}
}
