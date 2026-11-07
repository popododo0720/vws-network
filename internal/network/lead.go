package network

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/popododo0720/vws-lib/obs"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1 "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// leaseName is the coordination.k8s.io Lease in the store namespace. Its
// holder is the one replica that runs the workers and the reconcile loops.
const leaseName = "vws-network"

// leaseTiming is the client-go leader election timing. The zero value is the
// client-go recommended 15s lease, 10s renew deadline and 2s retry.
type leaseTiming struct {
	duration, renew, retry time.Duration
}

func (t leaseTiming) orDefault() leaseTiming {
	if t == (leaseTiming{}) {
		return leaseTiming{duration: 15 * time.Second, renew: 10 * time.Second, retry: 2 * time.Second}
	}
	return t
}

// elector runs lead while this process is the one writer of the store. It
// returns once ctx is done and lead has returned.
type elector func(ctx context.Context, lead func(context.Context))

// leaseElector competes for leaseName in ns. A replica that loses the Lease
// stops lead and competes again; Close stops lead before giving the Lease up,
// so the next holder never overlaps it.
func leaseElector(rc *rest.Config, ns string, t leaseTiming) (elector, error) {
	cs, err := coordinationv1.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("lease client: %w", err)
	}
	host, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("lease identity: %w", err)
	}
	t = t.orDefault()
	cfg := leaderelection.LeaderElectionConfig{
		Lock: &resourcelock.LeaseLock{
			LeaseMeta:  metav1.ObjectMeta{Name: leaseName, Namespace: ns},
			Client:     cs,
			LockConfig: resourcelock.ResourceLockConfig{Identity: randomID(host + "_")},
		},
		LeaseDuration:   t.duration,
		RenewDeadline:   t.renew,
		RetryPeriod:     t.retry,
		ReleaseOnCancel: true,
		Name:            leaseName,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(context.Context) {},
			OnStoppedLeading: func() {},
		},
	}
	if _, err := leaderelection.NewLeaderElector(cfg); err != nil {
		return nil, fmt.Errorf("lease election: %w", err)
	}
	return func(ctx context.Context, lead func(context.Context)) { elect(ctx, cfg, lead) }, nil
}

func elect(ctx context.Context, cfg leaderelection.LeaderElectionConfig, lead func(context.Context)) {
	electCtx, release := context.WithCancel(context.WithoutCancel(ctx))
	defer release()
	for {
		r := &round{}
		cfg.Callbacks.OnStartedLeading = func(c context.Context) { r.lead(c, ctx, lead) }
		le, err := leaderelection.NewLeaderElector(cfg)
		if err != nil {
			slog.Default().LogAttrs(ctx, slog.LevelError, "network: lease election", slog.String(obs.FieldErr, err.Error()))
			return
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			le.Run(electCtx)
		}()
		select {
		case <-done:
			r.finish()
			if ctx.Err() != nil {
				return
			}
			slog.Default().LogAttrs(ctx, slog.LevelWarn, "network: lost the lease", slog.String("lease", leaseName))
		case <-ctx.Done():
			r.finish()
			release()
			<-done
			return
		}
	}
}

// round is one client-go Run. client-go starts the leading callback in its
// own goroutine and does not wait for it, so round makes finish wait for a
// lead that started and keeps a late one from starting at all.
type round struct {
	mu   sync.Mutex
	over bool
	wg   sync.WaitGroup
}

func (r *round) lead(c, work context.Context, lead func(context.Context)) {
	r.mu.Lock()
	if r.over {
		r.mu.Unlock()
		return
	}
	r.wg.Add(1)
	r.mu.Unlock()
	defer r.wg.Done()
	c, stop := context.WithCancel(c)
	defer stop()
	defer context.AfterFunc(work, stop)()
	lead(c)
}

func (r *round) finish() {
	r.mu.Lock()
	r.over = true
	r.mu.Unlock()
	r.wg.Wait()
}

// takeOver is what a new Lease holder runs: operations the previous holder
// left running go back to pending, then the loops run until the Lease is lost
// or Close.
func (s *Service) takeOver(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		err := s.repo.requeue(ctx)
		if err == nil {
			break
		}
		if s.logf != nil {
			s.logf("network: requeue after taking the lease: %v", err)
		}
		t := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
	slog.Default().LogAttrs(ctx, slog.LevelInfo, "network: took the lease", slog.String("lease", leaseName))
	s.work(ctx)
}

// work runs every loop until ctx is done.
func (s *Service) work(ctx context.Context) {
	s.heartbeat.Store(s.now().UnixNano())
	s.leading.Store(true)
	defer s.leading.Store(false)
	var wg sync.WaitGroup
	for _, loop := range s.loops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			loop(ctx)
		}()
	}
	wg.Wait()
}
