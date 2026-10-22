package network

import (
	"context"
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

// sgPriority is the one OVN ACL priority every rule gets. Rules only allow, so order does not matter.
const sgPriority = 1

func (k *kubeOVN) ensureGroup(ctx context.Context, g domain.SecurityGroup) error {
	return k.ensure(ctx, gvkGroup, string(g.ID), map[string]any{
		"ingressRules":          []any{},
		"egressRules":           []any{},
		"allowSameGroupTraffic": false,
	}, "ingressRules", "egressRules")
}

func (k *kubeOVN) deleteGroup(ctx context.Context, g domain.SecurityGroup) error {
	return k.remove(ctx, gvkGroup, string(g.ID))
}

func (k *kubeOVN) ensureRule(ctx context.Context, g domain.SecurityGroupID, r domain.Rule) error {
	want, err := ovnRule(r)
	if err != nil {
		return err
	}
	field := ruleField(r.Direction)
	return k.editRules(ctx, g, true, field, func(rules []any) []any {
		for _, have := range rules {
			if reflect.DeepEqual(have, want) {
				return rules
			}
		}
		return append(rules, want)
	})
}

func (k *kubeOVN) deleteRule(ctx context.Context, g domain.SecurityGroupID, r domain.Rule) error {
	drop, err := ovnRule(r)
	if err != nil {
		return err
	}
	return k.editRules(ctx, g, false, ruleField(r.Direction), func(rules []any) []any {
		kept := make([]any, 0, len(rules))
		for _, have := range rules {
			if !reflect.DeepEqual(have, drop) {
				kept = append(kept, have)
			}
		}
		return kept
	})
}

// editRules replaces one rule list with a single Update, retried on conflict.
// A missing group is an error for ensure and success for delete.
func (k *kubeOVN) editRules(ctx context.Context, g domain.SecurityGroupID, mustExist bool, field string, edit func([]any) []any) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(gvkGroup)
		if err := k.c.Get(ctx, client.ObjectKey{Name: string(g)}, o); err != nil {
			if !mustExist {
				return ignoreNotFound("editRules", err)
			}
			return fmt.Errorf("editRules: %w", err)
		}
		have, _, err := unstructured.NestedSlice(o.Object, "spec", field)
		if err != nil {
			return fmt.Errorf("editRules: %w", err)
		}
		next := edit(have)
		if len(next) == len(have) && reflect.DeepEqual(next, have) {
			return nil
		}
		if err := unstructured.SetNestedSlice(o.Object, next, "spec", field); err != nil {
			return fmt.Errorf("editRules: %w", err)
		}
		return k.c.Update(ctx, o)
	})
	if err != nil {
		return fmt.Errorf("editRules %s: %w", g, err)
	}
	return nil
}

func ruleField(d domain.Direction) string {
	if d == domain.Egress {
		return "egressRules"
	}
	return "ingressRules"
}

// ovnRule converts a domain rule to a Kube-OVN SecurityGroup rule.
// Kube-OVN has no port 0 for tcp and udp and no field for an ICMP type or
// code, so such a rule is rejected instead of widened.
func ovnRule(r domain.Rule) (map[string]any, error) {
	if err := domain.CheckRule(r); err != nil {
		return nil, err
	}
	out := map[string]any{
		"ipVersion": "ipv4",
		"priority":  int64(sgPriority),
		"policy":    "allow",
	}
	switch r.Protocol {
	case domain.TCP, domain.UDP:
		if r.FromPort == 0 {
			return nil, &domain.Error{Kind: domain.Invalid, Msg: "kube-ovn does not support port 0 in a tcp or udp rule"}
		}
		out["protocol"] = map[domain.Protocol]string{domain.TCP: "tcp", domain.UDP: "udp"}[r.Protocol]
		out["portRangeMin"] = int64(r.FromPort)
		out["portRangeMax"] = int64(r.ToPort)
	case domain.ICMP:
		if r.FromPort != -1 || r.ToPort != -1 {
			return nil, &domain.Error{Kind: domain.Invalid, Msg: "kube-ovn cannot match an icmp type or code"}
		}
		out["protocol"] = "icmp"
	case domain.All:
		out["protocol"] = "all"
	}
	if r.Peer.Group != "" {
		out["remoteType"] = "securityGroup"
		out["remoteSecurityGroup"] = string(r.Peer.Group)
	} else {
		out["remoteType"] = "address"
		out["remoteAddress"] = r.Peer.CIDR.String()
	}
	return out, nil
}
