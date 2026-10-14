package network

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

// vmiRelist bounds how long a broken watch goes unnoticed: the loop lists
// again after it, which also catches events missed between watches.
const vmiRelist = 5 * time.Minute

// vmiLoop lists the VMIs in the VM namespace, follows each, then watches
// them from that list and follows every Added or Modified VMI. A closed or
// failed watch lists again after a second. It uses only VMI list and watch.
func (s *Service) vmiLoop(ctx context.Context, k *kubeOVN) {
	for ctx.Err() == nil {
		if err := s.vmiWatchOnce(ctx, k); err != nil && ctx.Err() == nil && s.logf != nil {
			s.logf("network: vmi watch: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (s *Service) vmiWatchOnce(ctx context.Context, k *kubeOVN) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvkVMI.GroupVersion().WithKind("VirtualMachineInstanceList"))
	if err := k.w.List(ctx, list, client.InNamespace(k.vmNamespace)); err != nil {
		return fmt.Errorf("list vmis in %s: %w", k.vmNamespace, err)
	}
	for i := range list.Items {
		s.followVMI(ctx, &list.Items[i])
	}
	wctx, cancel := context.WithTimeout(ctx, vmiRelist)
	defer cancel()
	w, err := k.w.Watch(wctx, list, client.InNamespace(k.vmNamespace), &client.ListOptions{Raw: rawWatchFrom(list.GetResourceVersion())})
	if err != nil {
		return fmt.Errorf("watch vmis in %s: %w", k.vmNamespace, err)
	}
	defer w.Stop()
	for ev := range w.ResultChan() {
		if ev.Type != watch.Added && ev.Type != watch.Modified {
			continue
		}
		if vmi, ok := ev.Object.(*unstructured.Unstructured); ok {
			s.followVMI(ctx, vmi)
		}
	}
	return nil
}

// followVMI moves every address attached to an interface of vmi whose
// reported IP changed. An interface with no IP (a VM restarting) leaves the
// address and its OvnFip alone until a new IP is reported. Addresses with no
// private address yet are left to the worker's resolve.
func (s *Service) followVMI(ctx context.Context, vmi *unstructured.Unstructured) {
	ids, err := s.repo.projectIDs(ctx)
	if err != nil {
		if s.logf != nil {
			s.logf("network: follow vmi %s: %v", vmi.GetName(), err)
		}
		return
	}
	for _, p := range ids {
		st, _, err := s.repo.load(ctx, p)
		if err != nil {
			if s.logf != nil {
				s.logf("network: follow vmi %s: %v", vmi.GetName(), err)
			}
			continue
		}
		for _, e := range st.ElasticIPs {
			if !e.Private.IsValid() || e.Instance[strings.LastIndex(e.Instance, "/")+1:] != vmi.GetName() {
				continue
			}
			addr, err := netip.ParseAddr(vmiInterfaceIP(vmi, e.Device))
			if err != nil || addr == e.Private {
				continue
			}
			if err := s.moveInterfaceAddress(ctx, p, e.ID, addr, vmi.GetResourceVersion()); err != nil && s.logf != nil {
				s.logf("network: follow vmi %s for %s: %v", vmi.GetName(), e.ID, err)
			}
		}
	}
}

// moveBody is the fingerprint of a followed IP change.
type moveBody struct {
	ElasticIP domain.ElasticIPID `json:"elastic_ip"`
	Address   string             `json:"address"`
	VMIRev    string             `json:"vmi_resource_version"`
}

// moveInterfaceAddress records addr as the private address of id and queues
// the NAT change, as an UpdateElasticIp operation. The key names the VMI
// resourceVersion, so a later change back to an earlier IP is a new operation.
func (s *Service) moveInterfaceAddress(ctx context.Context, p domain.ProjectID, id domain.ElasticIPID, addr netip.Addr, rev string) error {
	key := domain.IdempotencyKey("vmi-ip-" + string(id) + "-" + rev)
	body := moveBody{ElasticIP: id, Address: addr.String(), VMIRev: rev}
	_, err := s.mutate(ctx, domain.Call{Project: p, Key: key}, domain.OpUpdateElasticIP, string(id), body, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		return st.MoveInterfaceAddress(id, addr)
	})
	return err
}
