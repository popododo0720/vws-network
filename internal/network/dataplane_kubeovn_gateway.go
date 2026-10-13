package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// gatewayLabel marks a node as an OVN external gateway. kube-ovn-controller
// rewrites it to "false" when it restarts, which breaks SNAT; the
// ovn-external-gw-config ConfigMap that would pin it also breaks underlay, so
// this service puts it back instead.
const gatewayLabel = "ovn.kubernetes.io/external-gw"

// defaultGatewayRecheck is how often the gateway labels are checked.
const defaultGatewayRecheck = 30 * time.Second

// labelGatewayNodes sets gatewayLabel=true on every node named by
// -kube-gateway-nodes whose label is anything else. It never touches another
// node and never removes a label. It returns the nodes it changed. A missing
// node is an error for that node only.
func (k *kubeOVN) labelGatewayNodes(ctx context.Context) ([]string, error) {
	var fixed []string
	var errs []error
	for _, name := range k.gatewayNodes {
		n := &corev1.Node{}
		if err := k.c.Get(ctx, client.ObjectKey{Name: name}, n); err != nil {
			errs = append(errs, fmt.Errorf("get gateway node %s: %w", name, err))
			continue
		}
		if n.Labels[gatewayLabel] == "true" {
			continue
		}
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{gatewayLabel: "true"}}})
		if err != nil {
			return fixed, fmt.Errorf("label gateway node %s: %w", name, err)
		}
		if err := k.c.Patch(ctx, n, client.RawPatch(types.MergePatchType, patch)); err != nil {
			errs = append(errs, fmt.Errorf("label gateway node %s: %w", name, err))
			continue
		}
		fixed = append(fixed, name)
	}
	return fixed, errors.Join(errs...)
}

// gatewayLoop rechecks the gateway labels every period while any VPC exists.
// Every VPC this service makes is attached to the external network, so one
// VPC is enough to need the gateways.
func (s *Service) gatewayLoop(ctx context.Context, k *kubeOVN, period time.Duration) {
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		s.relabelGateways(ctx, k)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) relabelGateways(ctx context.Context, k *kubeOVN) {
	has, err := s.anyVPC(ctx)
	if err != nil || !has {
		if err != nil && s.logf != nil {
			s.logf("network: gateway labels: %v", err)
		}
		return
	}
	fixed, err := k.labelGatewayNodes(ctx)
	if len(fixed) > 0 && s.logf != nil {
		s.logf("network: restored %s=true on %v", gatewayLabel, fixed)
	}
	if err != nil && s.logf != nil {
		s.logf("network: gateway labels: %v", err)
	}
}

// anyVPC reports whether any project holds a VPC.
func (s *Service) anyVPC(ctx context.Context) (bool, error) {
	ids, err := s.repo.projectIDs(ctx)
	if err != nil {
		return false, err
	}
	for _, p := range ids {
		st, _, err := s.repo.load(ctx, p)
		if err != nil {
			return false, err
		}
		if len(st.VPCs) > 0 {
			return true, nil
		}
	}
	return false, nil
}
