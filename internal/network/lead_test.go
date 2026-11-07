package network

import (
	"context"
	"testing"
	"time"

	"github.com/popododo0720/vws-network/internal/domain"
)

// TestRoundLateLeadDoesNotRun: client-go starts the leading callback in a
// goroutine it does not wait for. One that starts after its round finished
// must not run, or two leads would overlap in one process.
func TestRoundLateLeadDoesNotRun(t *testing.T) {
	r := &round{}
	r.finish()
	ran := false
	r.lead(context.Background(), context.Background(), func(context.Context) { ran = true })
	if ran {
		t.Fatal("a lead that started after finish ran")
	}
}

// TestRoundFinishWaitsForLead: finish returns only after a running lead has
// returned, and stopping the work context stops that lead.
func TestRoundFinishWaitsForLead(t *testing.T) {
	r := &round{}
	work, stop := context.WithCancel(context.Background())
	started, returned := make(chan struct{}), make(chan struct{})
	go r.lead(context.Background(), work, func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond)
		close(returned)
	})
	<-started
	stop()
	r.finish()
	select {
	case <-returned:
	default:
		t.Fatal("finish returned before the lead")
	}
}

// TestTakeOverRequeuesBeforeWork: a new Lease holder returns operations the
// previous holder left running to pending before any worker claims.
func TestTakeOverRequeuesBeforeWork(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	svc, err := build(repo, newFake(), mustP("10.99.0.0/24"), 0)
	if err != nil {
		t.Fatal(err)
	}
	op, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.8.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repo.claim(ctx, svc.now(), "old-holder"); err != nil || !ok {
		t.Fatal(err, ok)
	}
	leadCtx, cancel := context.WithCancel(ctx)
	svc.loops = []func(context.Context){func(context.Context) { cancel() }}
	svc.takeOver(leadCtx)
	got, err := svc.GetOperation(ctx, "proj", op.ID)
	if err != nil || got.State != domain.OpPending {
		t.Fatalf("after take over %+v err %v", got, err)
	}
}
