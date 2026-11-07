package network

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/popododo0720/vws-lib/obs"
	coordv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
// so the next holder never overlaps it. client-go's ReleaseOnCancel is off:
// after a failed renewal it would first try a release, for up to the renew
// deadline, while lead still runs and the Lease runs out.
func leaseElector(ctx context.Context, rc *rest.Config, ns string, t leaseTiming) (elector, error) {
	cs, err := coordinationv1.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("lease client: %w", err)
	}
	if err := probeLease(ctx, cs.Leases(ns)); err != nil {
		return nil, err
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
		LeaseDuration: t.duration,
		RenewDeadline: t.renew,
		RetryPeriod:   t.retry,
		Name:          leaseName,
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

// probeLease refuses start when the Lease cannot be read, created and
// updated. Without it every replica would be ready and none would ever run
// an operation.
func probeLease(ctx context.Context, leases coordinationv1.LeaseInterface) error {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	dry := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	probe := &coordv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: leaseName}}
	err := retried(func() error {
		if _, err := leases.Create(ctx, probe, dry); err != nil {
			return fmt.Errorf("dry-run create: %w", err)
		}
		return nil
	})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("lease %s: %w", leaseName, err)
	}
	var cur *coordv1.Lease
	err = retried(func() error {
		var err error
		if cur, err = leases.Get(ctx, leaseName, metav1.GetOptions{}); err != nil {
			return fmt.Errorf("get: %w", err)
		}
		return nil
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err == nil {
		err = retried(func() error {
			if _, err := leases.Update(ctx, cur, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
				return fmt.Errorf("dry-run update: %w", err)
			}
			return nil
		})
	}
	if err != nil {
		return fmt.Errorf("lease %s: %w", leaseName, err)
	}
	return nil
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
			giveUp(ctx, cfg)
			return
		}
	}
}

// giveUp clears the Lease when this replica still holds it, so the next
// replica takes it at once instead of after the lease duration. Run has
// returned, so nothing renews it any more.
func giveUp(ctx context.Context, cfg leaderelection.LeaderElectionConfig) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.RenewDeadline)
	defer cancel()
	rec, _, err := cfg.Lock.Get(ctx)
	if err == nil && rec.HolderIdentity == cfg.Lock.Identity() {
		now := metav1.NewTime(time.Now())
		err = cfg.Lock.Update(ctx, resourcelock.LeaderElectionRecord{
			LeaseDurationSeconds: 1,
			AcquireTime:          now,
			RenewTime:            now,
			LeaderTransitions:    rec.LeaderTransitions,
		})
	}
	if err != nil {
		slog.Default().LogAttrs(ctx, slog.LevelWarn, "network: give up the lease", slog.String(obs.FieldErr, err.Error()))
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
