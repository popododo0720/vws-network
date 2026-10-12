package network

import (
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

// KubeOVNConfig selects the Kube-OVN dataplane. An empty Kubeconfig selects
// the in-process fake and every other field must then be empty.
// ExternalSubnet names the Kube-OVN Subnet that holds the elastic IP pool.
// NADNamespace holds one NetworkAttachmentDefinition per subnet.
type KubeOVNConfig struct {
	Kubeconfig     string
	ExternalSubnet string
	NADNamespace   string
	// SNAT gives every subnet outbound SNAT and needs ExternalSubnet.
	SNAT bool
}

// newDataplane is the one place a Config becomes a dataplane.
func newDataplane(cfg KubeOVNConfig) (dataplane, error) {
	if cfg.Kubeconfig == "" {
		if cfg.ExternalSubnet != "" || cfg.NADNamespace != "" || cfg.SNAT {
			return nil, &domain.Error{Kind: domain.Invalid, Msg: "kube-ovn options need a kubeconfig"}
		}
		return newFake(), nil
	}
	if cfg.NADNamespace == "" {
		return nil, &domain.Error{Kind: domain.Invalid, Msg: "kube-ovn nad namespace is required"}
	}
	if cfg.SNAT && cfg.ExternalSubnet == "" {
		return nil, &domain.Error{Kind: domain.Invalid, Msg: "kube-ovn snat needs an external subnet"}
	}
	rc, err := clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
	if err != nil {
		return nil, err
	}
	c, err := client.New(rc, client.Options{})
	if err != nil {
		return nil, err
	}
	return &kubeOVN{c: c, externalSubnet: cfg.ExternalSubnet, nadNamespace: cfg.NADNamespace, snat: cfg.SNAT}, nil
}
