package network

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/popododo0720/vws-network/internal/domain"
)

var (
	errStale        = errors.New("network: project version moved")
	errKeyTaken     = errors.New("network: idempotency key taken")
	errAddressTaken = errors.New("network: pool address taken")
	errLostClaim    = errors.New("network: claim token mismatch")
)

// repository is the only store of desired state, operations, and pool addresses.
// memRepo and sqliteRepo both implement it. A commit is atomic.
type repository interface {
	load(ctx context.Context, p domain.ProjectID) (domain.State, int64, error)
	addressesInUse(ctx context.Context) (map[netip.Addr]bool, error)
	key(ctx context.Context, p domain.ProjectID, k domain.IdempotencyKey) (job, bool, error)
	operation(ctx context.Context, p domain.ProjectID, id string) (domain.Operation, error)
	commit(ctx context.Context, c commit) error
	claim(ctx context.Context, now time.Time, token string) (job, bool, error)
	requeue(ctx context.Context) error
	close() error
}

// commit is one atomic write. A nil state leaves the project untouched.
// enqueue and settle are mutually exclusive. settle must carry the claim token.
type commit struct {
	project domain.ProjectID
	base    int64
	state   *domain.State
	enqueue *job
	settle  *job
}

// job is one operation plus the fields the worker needs.
type job struct {
	op          domain.Operation
	fingerprint [32]byte
	lane        domain.VPCID
	seq         int64
	attempts    int
	notBefore   time.Time
	token       string
}

// pickRunnable returns the lowest-sequence pending job whose lane has no
// earlier non-terminal job and whose notBefore has arrived.
func pickRunnable(jobs []job, now time.Time) (job, bool) {
	blocked := map[domain.VPCID]bool{}
	order := append([]job(nil), jobs...)
	sortJobs(order)
	for _, j := range order {
		if j.op.State == domain.OpSucceeded || j.op.State == domain.OpFailed || j.lane == "" {
			continue
		}
		if j.op.State == domain.OpRunning || j.notBefore.After(now) {
			blocked[j.lane] = true
			continue
		}
		if blocked[j.lane] {
			continue
		}
		return j, true
	}
	return job{}, false
}

func sortJobs(jobs []job) {
	for i := 1; i < len(jobs); i++ {
		for k := i; k > 0 && jobs[k].seq < jobs[k-1].seq; k-- {
			jobs[k], jobs[k-1] = jobs[k-1], jobs[k]
		}
	}
}

func addressesOf(st domain.State) map[netip.Addr]bool {
	out := map[netip.Addr]bool{}
	for _, e := range st.ElasticIPs {
		if e.Address.IsValid() {
			out[e.Address] = true
		}
	}
	return out
}

func conflictingAddress(existing map[netip.Addr]domain.ProjectID, project domain.ProjectID, st domain.State) bool {
	for addr := range addressesOf(st) {
		owner, ok := existing[addr]
		if ok && owner != project {
			return true
		}
	}
	return false
}

func nano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromNano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}
