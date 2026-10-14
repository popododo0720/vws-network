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
	// VMMTU is the MTU of VM interfaces in VPC subnets: Subnet spec.mtu, which
	// Kube-OVN also hands out as DHCP option 26. 0 means DefaultVMMTU. The
	// chart sets it from VWSCluster spec.network.vmMTU (vws-deploy#236).
	VMMTU int
}

// DefaultVMMTU and the accepted VMMTU range. 9001 is the AWS VPC MTU; 68 is
// the IPv4 minimum (RFC 791).
const (
	DefaultVMMTU = 9001
	MinVMMTU     = 68
	MaxVMMTU     = 9001
)

// CheckVMMTU rejects an MTU outside MinVMMTU..MaxVMMTU. 0 is the default.
func CheckVMMTU(mtu int) error {
	if mtu != 0 && (mtu < MinVMMTU || mtu > MaxVMMTU) {
		return &domain.Error{Kind: domain.Invalid, Msg: fmt.Sprintf("vm mtu %d is outside %d..%d", mtu, MinVMMTU, MaxVMMTU)}
	}
	return nil
}

const defaultNADNamespace = "vws-system"

// inClusterConfig is a variable so tests can stand in for a pod.
var inClusterConfig = rest.InClusterConfig

func (c KubeOVNConfig) selected() bool {
	return c.Kubeconfig != "" || c.ExternalSubnet != "" || c.NADNamespace != "" || c.SNAT || c.Underlay.enabled() || len(c.Direct) > 0 || len(c.GatewayNodes) > 0
}

// newDataplane is the one place a Config becomes a dataplane.
func newDataplane(cfg KubeOVNConfig) (dataplane, error) {
	if err := CheckVMMTU(cfg.VMMTU); err != nil {
		return nil, err
	}
	if cfg.VMMTU == 0 {
		cfg.VMMTU = DefaultVMMTU
	}
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
	if cfg.GatewayRecheck < 0 || (cfg.GatewayRecheck > 0 && cfg.GatewayRecheck < minGatewayRecheck) {
		return nil, &domain.Error{Kind: domain.Invalid, Msg: fmt.Sprintf("-kube-gateway-recheck %v must be 0 (the %v default) or at least %v", cfg.GatewayRecheck, defaultGatewayRecheck, minGatewayRecheck)}
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
	c, err := client.NewWithWatch(rc, client.Options{})
	if err != nil {
		return nil, fmt.Errorf("newDataplane: %w", err)
	}
	return &kubeOVN{c: c, externalSubnet: cfg.ExternalSubnet, nadNamespace: cfg.NADNamespace, snat: cfg.SNAT, underlay: cfg.Underlay, direct: cfg.Direct, directNADNamespace: cfg.DirectNADNamespace, vmNamespace: cfg.VMNamespace, w: c, gatewayNodes: cfg.GatewayNodes, vmMTU: cfg.VMMTU}, nil
}
