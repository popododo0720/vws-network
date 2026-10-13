package network

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/popododo0720/vws-network/internal/domain"
)

type memProject struct {
	version int64
	state   domain.State
}

// memRepo is an in-memory repository. Tests share one value across a restart.
type memRepo struct {
	mu        chan struct{}
	projects  map[domain.ProjectID]memProject
	jobs      []job
	addresses map[netip.Addr]domain.ProjectID
	nextSeq   int64
}

func newMemRepo() *memRepo {
	return &memRepo{
		mu:        make(chan struct{}, 1),
		projects:  map[domain.ProjectID]memProject{},
		addresses: map[netip.Addr]domain.ProjectID{},
	}
}

func (r *memRepo) lock(ctx context.Context) error {
	select {
	case r.mu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("memRepo.lock: %w", ctx.Err())
	}
}

func (r *memRepo) unlock() { <-r.mu }

func (r *memRepo) close() error { return nil }

func (r *memRepo) ping(ctx context.Context) error {
	if err := r.lock(ctx); err != nil {
		return err
	}
	r.unlock()
	return nil
}

func (r *memRepo) load(ctx context.Context, p domain.ProjectID) (domain.State, int64, error) {
	if err := r.lock(ctx); err != nil {
		return domain.State{}, 0, err
	}
	defer r.unlock()
	proj, ok := r.projects[p]
	if !ok {
		return domain.State{}, 0, nil
	}
	return proj.state.Clone(), proj.version, nil
}

func (r *memRepo) addressesInUse(ctx context.Context) (map[netip.Addr]bool, error) {
	if err := r.lock(ctx); err != nil {
		return nil, err
	}
	defer r.unlock()
	out := make(map[netip.Addr]bool, len(r.addresses))
	for addr := range r.addresses {
		out[addr] = true
	}
	return out, nil
}

func (r *memRepo) projectIDs(ctx context.Context) ([]domain.ProjectID, error) {
	if err := r.lock(ctx); err != nil {
		return nil, err
	}
	defer r.unlock()
	out := make([]domain.ProjectID, 0, len(r.projects))
	for p := range r.projects {
		out = append(out, p)
	}
	return out, nil
}

func (r *memRepo) pending(ctx context.Context) (int64, error) {
	if err := r.lock(ctx); err != nil {
		return 0, err
	}
	defer r.unlock()
	var n int64
	for _, j := range r.jobs {
		if j.op.State == domain.OpPending || j.op.State == domain.OpRunning {
			n++
		}
	}
	return n, nil
}

func (r *memRepo) key(ctx context.Context, p domain.ProjectID, k domain.IdempotencyKey) (job, bool, error) {
	if err := r.lock(ctx); err != nil {
		return job{}, false, err
	}
	defer r.unlock()
	for _, j := range r.jobs {
		if j.op.Project == p && j.op.Key == k {
			return j, true, nil
		}
	}
	return job{}, false, nil
}

func (r *memRepo) operation(ctx context.Context, p domain.ProjectID, id string) (domain.Operation, error) {
	if err := r.lock(ctx); err != nil {
		return domain.Operation{}, err
	}
	defer r.unlock()
	for _, j := range r.jobs {
		if j.op.Project == p && j.op.ID == id {
			return j.op, nil
		}
	}
	return domain.Operation{}, &domain.Error{Kind: domain.NotFound, Msg: "operation not found"}
}

func (r *memRepo) commit(ctx context.Context, c commit) error {
	if err := r.lock(ctx); err != nil {
		return err
	}
	defer r.unlock()
	if c.enqueue != nil && c.settle != nil {
		return errLostClaim
	}
	proj := r.projects[c.project]
	if c.state != nil && proj.version != c.base {
		return errStale
	}
	if c.state != nil && conflictingAddress(r.addresses, c.project, *c.state) {
		return errAddressTaken
	}
	if c.enqueue != nil {
		for _, j := range r.jobs {
			if j.op.Project == c.project && j.op.Key == c.enqueue.op.Key {
				return errKeyTaken
			}
		}
	}
	idx := -1
	if c.settle != nil {
		for i := range r.jobs {
			if r.jobs[i].op.ID == c.settle.op.ID {
				idx = i
				break
			}
		}
		if idx < 0 || r.jobs[idx].token != c.settle.token {
			return errLostClaim
		}
		if r.jobs[idx].op.State == domain.OpSucceeded || r.jobs[idx].op.State == domain.OpFailed {
			return nil
		}
	}
	if c.state != nil {
		proj.version++
		proj.state = c.state.Clone()
		r.projects[c.project] = proj
		for addr, owner := range r.addresses {
			if owner == c.project {
				delete(r.addresses, addr)
			}
		}
		for addr := range addressesOf(proj.state) {
			r.addresses[addr] = c.project
		}
	}
	if c.enqueue != nil {
		r.nextSeq++
		j := *c.enqueue
		j.seq = r.nextSeq
		r.jobs = append(r.jobs, j)
	}
	if c.settle != nil {
		cur := r.jobs[idx]
		cur.op.State = c.settle.op.State
		cur.op.Err = c.settle.op.Err
		cur.attempts = c.settle.attempts
		cur.notBefore = c.settle.notBefore
		if c.settle.op.State == domain.OpPending {
			cur.token = ""
		}
		r.jobs[idx] = cur
	}
	return nil
}

func (r *memRepo) claim(ctx context.Context, now time.Time, token string) (job, bool, error) {
	if err := r.lock(ctx); err != nil {
		return job{}, false, err
	}
	defer r.unlock()
	j, ok := pickRunnable(r.jobs, now)
	if !ok {
		return job{}, false, nil
	}
	for i := range r.jobs {
		if r.jobs[i].seq != j.seq {
			continue
		}
		r.jobs[i].op.State = domain.OpRunning
		r.jobs[i].token = token
		return r.jobs[i], true, nil
	}
	return job{}, false, nil
}

func (r *memRepo) requeue(ctx context.Context) error {
	if err := r.lock(ctx); err != nil {
		return err
	}
	defer r.unlock()
	for i := range r.jobs {
		if r.jobs[i].op.State == domain.OpRunning {
			r.jobs[i].op.State = domain.OpPending
			r.jobs[i].token = ""
		}
	}
	return nil
}
