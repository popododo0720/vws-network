package network

import (
	"context"
	"errors"
	"net/netip"
	"sync"

	"github.com/popododo0720/vws-network/internal/domain"
)

// fakeDataplane records the objects a dataplane would hold. It is the only
// dataplane implementation in this repo.
type fakeDataplane struct {
	mu      sync.Mutex
	objects map[string]struct{}
	calls   []string
	fails   int
	failErr error
}

func newFake() *fakeDataplane {
	return &fakeDataplane{objects: map[string]struct{}{}}
}

// failNext makes the next n calls return err and leave the record unchanged.
func (f *fakeDataplane) failNext(n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails = n
	f.failErr = err
}

func (f *fakeDataplane) has(object string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[object]
	return ok
}

func (f *fakeDataplane) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeDataplane) call(name string, mutate func()) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	if f.fails > 0 {
		f.fails--
		err := f.failErr
		if err == nil {
			err = errors.New("dataplane: call failed")
		}
		return err
	}
	mutate()
	return nil
}

func (f *fakeDataplane) put(key string) { f.objects[key] = struct{}{} }

func (f *fakeDataplane) drop(key string) { delete(f.objects, key) }

func (f *fakeDataplane) ensureVPC(_ context.Context, v domain.VPC) error {
	return f.call("ensure-vpc/"+string(v.ID), func() { f.put("vpc/" + string(v.ID)) })
}
func (f *fakeDataplane) deleteVPC(_ context.Context, v domain.VPC) error {
	return f.call("delete-vpc/"+string(v.ID), func() { f.drop("vpc/" + string(v.ID)) })
}
func (f *fakeDataplane) ensureSubnet(_ context.Context, s domain.Subnet) error {
	return f.call("ensure-subnet/"+string(s.ID), func() { f.put("subnet/" + string(s.ID)) })
}
func (f *fakeDataplane) deleteSubnet(_ context.Context, s domain.Subnet) error {
	return f.call("delete-subnet/"+string(s.ID), func() { f.drop("subnet/" + string(s.ID)) })
}
func (f *fakeDataplane) ensureGroup(_ context.Context, g domain.SecurityGroup) error {
	return f.call("ensure-sg/"+string(g.ID), func() { f.put("sg/" + string(g.ID)) })
}
func (f *fakeDataplane) deleteGroup(_ context.Context, g domain.SecurityGroup) error {
	return f.call("delete-sg/"+string(g.ID), func() { f.drop("sg/" + string(g.ID)) })
}
func (f *fakeDataplane) ensureRule(_ context.Context, g domain.SecurityGroupID, r domain.Rule) error {
	key := "rule/" + string(g) + "/" + r.Identity()
	return f.call("ensure-"+key, func() { f.put(key) })
}
func (f *fakeDataplane) deleteRule(_ context.Context, g domain.SecurityGroupID, r domain.Rule) error {
	key := "rule/" + string(g) + "/" + r.Identity()
	return f.call("delete-"+key, func() { f.drop(key) })
}
func (f *fakeDataplane) ensureElasticIP(_ context.Context, e domain.ElasticIP) error {
	return f.call("ensure-eip/"+string(e.ID), func() { f.put("eip/" + string(e.ID)) })
}
func (f *fakeDataplane) deleteElasticIP(_ context.Context, e domain.ElasticIP) error {
	return f.call("delete-eip/"+string(e.ID), func() { f.drop("eip/" + string(e.ID)) })
}
func (f *fakeDataplane) ensureNAT(_ context.Context, e domain.ElasticIP, private netip.Addr) error {
	key := "nat/" + string(e.ID) + "/" + private.String()
	return f.call("ensure-"+key, func() { f.put(key) })
}
func (f *fakeDataplane) deleteNAT(_ context.Context, e domain.ElasticIP, private netip.Addr) error {
	key := "nat/" + string(e.ID) + "/" + private.String()
	return f.call("delete-"+key, func() { f.drop(key) })
}

var _ dataplane = (*fakeDataplane)(nil)
