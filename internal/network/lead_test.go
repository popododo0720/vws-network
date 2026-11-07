package network

import (
	"context"
	"testing"
	"time"
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
