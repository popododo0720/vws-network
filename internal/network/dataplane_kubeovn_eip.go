package network

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/popododo0720/vws-network/internal/domain"
)

func (k *kubeOVN) ensureElasticIP(ctx context.Context, e domain.ElasticIP) error {
	if k.externalSubnet == "" {
		return &domain.Error{Kind: domain.Invalid, Msg: "kube-ovn external subnet is not configured"}
	}
	return k.ensure(ctx, gvkEip, string(e.ID), map[string]any{
		"externalSubnet": k.externalSubnet,
		"v4ip":           e.Address.String(),
		"type":           "nat",
	})
}

// deleteElasticIP refuses while an OvnFip still points at the address.
func (k *kubeOVN) deleteElasticIP(ctx context.Context, e domain.ElasticIP) error {
	fips := &unstructured.UnstructuredList{}
	fips.SetGroupVersionKind(gvkFip.GroupVersion().WithKind("OvnFipList"))
	if err := k.c.List(ctx, fips); err != nil {
		return err
	}
	for _, f := range fips.Items {
		if ref, _, _ := unstructured.NestedString(f.Object, "spec", "ovnEip"); ref == string(e.ID) {
			return fmt.Errorf("elastic ip %s is still bound by %s", e.ID, f.GetName())
		}
	}
	return k.remove(ctx, gvkEip, string(e.ID))
}

func (k *kubeOVN) ensureNAT(ctx context.Context, e domain.ElasticIP, private netip.Addr) error {
	return k.ensure(ctx, gvkFip, fipName(e.ID, private), map[string]any{
		"ovnEip": string(e.ID),
		"vpc":    string(e.VPC),
		"v4Ip":   private.String(),
	})
}

func (k *kubeOVN) deleteNAT(ctx context.Context, e domain.ElasticIP, private netip.Addr) error {
	return k.remove(ctx, gvkFip, fipName(e.ID, private))
}

// fipName is the one place an OvnFip name is made.
func fipName(e domain.ElasticIPID, private netip.Addr) string {
	return string(e) + "-" + strings.ReplaceAll(private.String(), ".", "-")
}
