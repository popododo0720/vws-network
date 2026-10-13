package network

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/popododo0720/vws-network/internal/domain"
)

// nadName is the one place a NetworkAttachmentDefinition name is made.
// An instance attaches with the name "<nadNamespace>/<nadName>".
func nadName(s domain.SubnetID) string { return string(s) }

// nadProvider is the Kube-OVN provider string that ties a Subnet to its NAD.
func (k *kubeOVN) nadProvider(s domain.SubnetID) string {
	return providerIn(k.nadNamespace, s)
}

// providerIn is the provider string of the NAD named for s in namespace ns.
func providerIn(ns string, s domain.SubnetID) string {
	return nadName(s) + "." + ns + ".ovn"
}

func (k *kubeOVN) nad(s domain.SubnetID) *unstructured.Unstructured {
	return nadIn(k.nadNamespace, s)
}

func nadIn(ns string, s domain.SubnetID) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvkNAD)
	o.SetNamespace(ns)
	o.SetName(nadName(s))
	return o
}

func (k *kubeOVN) ensureNAD(ctx context.Context, s domain.SubnetID) error {
	return k.ensureNADIn(ctx, k.nadNamespace, s)
}

// ensureNADIn makes the NAD for s in ns. Tenant subnets use -kube-nad-namespace;
// direct networks use the VM namespace (-kube-direct-nad-namespace).
func (k *kubeOVN) ensureNADIn(ctx context.Context, ns string, s domain.SubnetID) error {
	if ns == "" {
		return &domain.Error{Kind: domain.Invalid, Msg: "kube-ovn nad namespace is not configured"}
	}
	cfg, err := json.Marshal(map[string]any{
		"cniVersion":    "0.3.1",
		"type":          "kube-ovn",
		"server_socket": "/run/openvswitch/kube-ovn-daemon.sock",
		"provider":      providerIn(ns, s),
	})
	if err != nil {
		return fmt.Errorf("ensureNAD: %w", err)
	}
	return k.ensureObject(ctx, nadIn(ns, s), string(s), map[string]any{"config": string(cfg)})
}

func (k *kubeOVN) removeNAD(ctx context.Context, s domain.SubnetID) error {
	return k.removeObject(ctx, k.nad(s), string(s))
}
