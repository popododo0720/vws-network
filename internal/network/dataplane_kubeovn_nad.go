package network

import (
	"context"
	"encoding/json"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

// nadName is the one place a NetworkAttachmentDefinition name is made.
// An instance attaches with the name "<nadNamespace>/<nadName>".
func nadName(s domain.SubnetID) string { return string(s) }

// nadProvider is the Kube-OVN provider string that ties a Subnet to its NAD.
func (k *kubeOVN) nadProvider(s domain.SubnetID) string {
	return nadName(s) + "." + k.nadNamespace + ".ovn"
}

func (k *kubeOVN) nad(s domain.SubnetID) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvkNAD)
	o.SetNamespace(k.nadNamespace)
	o.SetName(nadName(s))
	return o
}

func (k *kubeOVN) ensureNAD(ctx context.Context, s domain.SubnetID) error {
	if k.nadNamespace == "" {
		return &domain.Error{Kind: domain.Invalid, Msg: "kube-ovn nad namespace is not configured"}
	}
	cfg, err := json.Marshal(map[string]any{
		"cniVersion":    "0.3.1",
		"type":          "kube-ovn",
		"server_socket": "/run/openvswitch/kube-ovn-daemon.sock",
		"provider":      k.nadProvider(s),
	})
	if err != nil {
		return err
	}
	return k.ensureObject(ctx, k.nad(s), string(s), map[string]any{"config": string(cfg)})
}

func (k *kubeOVN) removeNAD(ctx context.Context, s domain.SubnetID) error {
	return client.IgnoreNotFound(k.c.Delete(ctx, k.nad(s)))
}
