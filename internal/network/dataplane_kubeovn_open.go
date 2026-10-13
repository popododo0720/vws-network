package network

import (
	"fmt"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

// KubeOVNConfig selects the Kube-OVN dataplane. A Kubeconfig selects it and
// takes precedence. Without one, any other field selects it through the
// in-cluster ServiceAccount. With neither, the in-process fake is used.
// ExternalSubnet is required once Kube-OVN is selected. It names the Kube-OVN
// Subnet that holds the elastic IP pool. NADNamespace holds one
// NetworkAttachmentDefinition per subnet and defaults to defaultNADNamespace.
type KubeOVNConfig struct {
	Kubeconfig     string
	ExternalSubnet string
	NADNamespace   string
	// SNAT gives every subnet outbound SNAT through ExternalSubnet.
	SNAT bool
	// Underlay, when set, makes the service create ExternalSubnet on a node
	// NIC instead of expecting the operator to have made it.
	Underlay UnderlayConfig
	// Direct are the direct provider networks this service creates at start
	// and exposes read-only to every project (vws-deploy#125).
	Direct []DirectNetwork
	// DirectNADNamespace is the VM namespace where the direct NADs live
	// (vws-deploy#125). Required with Direct; it has no default.
	DirectNADNamespace string
	// VMNamespace is the namespace of the instance VMIs (vws-compute's VM
	// namespace). An address attached to an instance reads its VMI there.
	VMNamespace string
	// GatewayNodes get ovn.kubernetes.io/external-gw=true, rechecked every
	// GatewayRecheck (default 5s) while any VPC exists. No other node is touched.
	GatewayNodes   []string
	GatewayRecheck time.Duration
}

const defaultNADNamespace = "vws-system"

// inClusterConfig is a variable so tests can stand in for a pod.
var inClusterConfig = rest.InClusterConfig

func (c KubeOVNConfig) selected() bool {
	return c.Kubeconfig != "" || c.ExternalSubnet != "" || c.NADNamespace != "" || c.SNAT || c.Underlay.enabled() || len(c.Direct) > 0 || len(c.GatewayNodes) > 0
}

// newDataplane is the one place a Config becomes a dataplane.
func newDataplane(cfg KubeOVNConfig) (dataplane, error) {
	if !cfg.selected() {
		return newFake(), nil
	}
	if cfg.NADNamespace == "" {
		cfg.NADNamespace = defaultNADNamespace
	}
	if cfg.ExternalSubnet == "" {
		return nil, &domain.Error{Kind: domain.Invalid, Msg: "kube-ovn external subnet is required"}
	}
	if len(cfg.Direct) > 0 && cfg.DirectNADNamespace == "" {
		return nil, &domain.Error{Kind: domain.Invalid, Msg: "kube-ovn direct networks need the VM namespace for their NADs (-kube-direct-nad-namespace)"}
	}
	if cfg.Underlay.enabled() {
		if err := cfg.Underlay.validate(); err != nil {
			return nil, err
		}
	}
	var rc *rest.Config
	var err error
	if cfg.Kubeconfig != "" {
		rc, err = clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
	} else if rc, err = inClusterConfig(); err != nil {
		err = fmt.Errorf("kube-ovn needs -kubeconfig or an in-cluster service account: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("newDataplane: %w", err)
	}
	c, err := client.New(rc, client.Options{})
	if err != nil {
		return nil, fmt.Errorf("newDataplane: %w", err)
	}
	return &kubeOVN{c: c, externalSubnet: cfg.ExternalSubnet, nadNamespace: cfg.NADNamespace, snat: cfg.SNAT, underlay: cfg.Underlay, direct: cfg.Direct, directNADNamespace: cfg.DirectNADNamespace, vmNamespace: cfg.VMNamespace, gatewayNodes: cfg.GatewayNodes}, nil
}
