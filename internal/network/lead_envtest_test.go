//go:build envtest

package network

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/popododo0720/vws-network/internal/domain"
)

// fastLease keeps a dead holder's Lease for 2s instead of 15s. The Lease
// record holds whole seconds.
var fastLease = leaseTiming{duration: 2 * time.Second, renew: 1500 * time.Millisecond, retry: 200 * time.Millisecond}

const leaseHolderEnv = "VWS_TEST_LEASE_HOLDER_KUBECONFIG"

// storeKubeconfig is a kubeconfig for a user that has only the store Role of
// docs/rbac.md.
func storeKubeconfig(t *testing.T, env *envtest.Environment, admin client.Client) string {
	t.Helper()
	ctx := context.Background()
	name := "vws-network-lease"
	subj := []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: name, APIGroup: rbacv1.GroupName}}
	for _, o := range []client.Object{
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: envNamespace}, Rules: storeRules},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: envNamespace}, Subjects: subj,
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		},
	} {
		if err := admin.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	u, err := env.AddUser(envtest.User{Name: name}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := u.KubeConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.Load(b)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*cfg, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func openReplica(t *testing.T, kubeconfig string) (*Service, *fakeDataplane) {
	t.Helper()
	svc, err := Open(context.Background(), Config{
		Store:       StoreConfig{Namespace: envNamespace, Kubeconfig: kubeconfig, lease: fastLease},
		AddressPool: envPool,
		Workers:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := svc.dp.(measured)
	if !ok {
		t.Fatalf("dataplane %T", svc.dp)
	}
	fake, ok := m.dataplane.(*fakeDataplane)
	if !ok {
		t.Fatalf("dataplane %T", m.dataplane)
	}
	return svc, fake
}

func leaseHolder(t *testing.T, c client.Client) string {
	t.Helper()
	var l coordinationv1.Lease
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: envNamespace, Name: leaseName}, &l); err != nil {
		return ""
	}
	if l.Spec.HolderIdentity == nil {
		return ""
	}
	return *l.Spec.HolderIdentity
}

func ensuredVPC(f *fakeDataplane, id string) int {
	n := 0
	for _, c := range f.log() {
		if c == "ensure-vpc/"+id {
			n++
		}
	}
	return n
}

// TestEnvtestLeaseOneWorkerAndHandoff opens two replicas as the store Role.
// The first runs the worker; the second accepts a write and leaves it to the
// first. Close hands the Lease over at once, and the two never lead together.
func TestEnvtestLeaseOneWorkerAndHandoff(t *testing.T) {
	env, admin := startEnv(t)
	ctx := context.Background()
	kc := storeKubeconfig(t, env, admin)
	first, firstDP := openReplica(t, kc)
	waitFor(t, first.leading.Load)
	second, secondDP := openReplica(t, kc)
	defer closeOrFail(t, second.Close)
	var overlap atomic.Bool
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			// second leads only after first stopped, so second first, then first.
			if second.leading.Load() && first.leading.Load() {
				overlap.Store(true)
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()

	op, err := second.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.8.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, second, "proj", op)
	if !firstDP.has("vpc/"+op.Resource) || len(secondDP.log()) != 0 {
		t.Fatalf("worker calls: first %v second %v", firstDP.log(), secondDP.log())
	}
	if second.leading.Load() {
		t.Fatal("second replica leads while the first holds the Lease")
	}
	if err := second.Ready(ctx); err != nil {
		t.Fatalf("follower not ready: %v", err)
	}

	start := time.Now()
	closeOrFail(t, first.Close)
	waitFor(t, second.leading.Load)
	if d := time.Since(start); d >= fastLease.duration {
		t.Fatalf("handoff after Close took %s, want under the %s lease", d, fastLease.duration)
	}
	sgReq := domain.CreateSecurityGroup{VPC: domain.VPCID(op.Resource), Name: "web"}
	sg, err := second.CreateSecurityGroup(ctx, call("proj", "sg"), sgReq)
	if err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, second, "proj", sg)
	if !secondDP.has("sg/" + sg.Resource) {
		t.Fatalf("new holder did not converge: %v", secondDP.log())
	}
	close(stop)
	<-sampled
	if overlap.Load() {
		t.Fatal("both replicas led at the same time")
	}
}

// stuckDataplane never finishes a VPC, so its holder keeps the operation
// RUNNING until it is killed.
type stuckDataplane struct{ *fakeDataplane }

func (stuckDataplane) ensureVPC(ctx context.Context, _ domain.VPC) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestEnvtestLeaseHolderProcess is the Lease holder that
// TestEnvtestLeaseHolderKilled runs in a child process and kills.
func TestEnvtestLeaseHolderProcess(t *testing.T) {
	kc := os.Getenv(leaseHolderEnv)
	if kc == "" {
		t.Skip("runs only as the child of TestEnvtestLeaseHolderKilled")
	}
	ctx := context.Background()
	rc, err := restConfig("lease holder", kc)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(rc, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := newKubeRepo(ctx, c, envNamespace)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := build(repo, stuckDataplane{newFake()}, envPool, 1)
	if err != nil {
		t.Fatal(err)
	}
	el, err := leaseElector(rc, envNamespace, fastLease)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.launch(ctx, el); err != nil {
		t.Fatal(err)
	}
	select {}
}

// TestEnvtestLeaseHolderKilled kills the Lease holder's process while it
// runs an operation. The other replica takes the Lease, requeues the
// operation, and finishes it once.
func TestEnvtestLeaseHolderKilled(t *testing.T) {
	env, admin := startEnv(t)
	ctx := context.Background()
	kc := storeKubeconfig(t, env, admin)
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEnvtestLeaseHolderProcess$")
	child.Env = append(os.Environ(), leaseHolderEnv+"="+kc)
	var out strings.Builder
	child.Stdout, child.Stderr = &out, &out
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := false
	t.Cleanup(func() {
		if !exited {
			_ = child.Process.Kill() //nolint:errcheck // cleanup of a child that may be gone
			_ = child.Wait()         //nolint:errcheck // cleanup of a child that may be gone
		}
	})
	waitFor(t, func() bool { return leaseHolder(t, admin) != "" })
	holder := leaseHolder(t, admin)

	replica, dp := openReplica(t, kc)
	defer closeOrFail(t, replica.Close)
	op, err := replica.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.8.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		got, err := replica.GetOperation(ctx, "proj", op.ID)
		return err == nil && got.State == domain.OpRunning
	})
	if replica.leading.Load() || len(dp.log()) != 0 || leaseHolder(t, admin) != holder {
		t.Fatalf("replica ran the worker under the child's Lease: calls %v, holder %q", dp.log(), leaseHolder(t, admin))
	}

	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait() //nolint:errcheck // a killed child exits with an error
	exited = true
	waitSucceeded(t, replica, "proj", op)
	if !replica.leading.Load() || leaseHolder(t, admin) == holder {
		t.Fatalf("replica finished without the Lease: holder %q", leaseHolder(t, admin))
	}
	if n := ensuredVPC(dp, op.Resource); n != 1 {
		t.Fatalf("vpc ensured %d times by the new holder, want 1: %v\nchild:\n%s", n, dp.log(), out.String())
	}
	vpcs, err := replica.ListVPCs(ctx, "proj")
	if err != nil || len(vpcs) != 1 || vpcs[0].Status != domain.StatusAvailable {
		t.Fatalf("vpcs %+v err %v", vpcs, err)
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestEnvtestLeaseLostStopsWorkersFirst fails every renewal and stalls every
// release of the holder. Its workers stop before the Lease runs out, so the
// next holder never runs beside them.
func TestEnvtestLeaseLostStopsWorkersFirst(t *testing.T) {
	env, admin := startEnv(t)
	ctx := context.Background()
	kc := storeKubeconfig(t, env, admin)
	rc, err := restConfig("lease holder", kc)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(rc, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := newKubeRepo(ctx, c, envNamespace)
	if err != nil {
		t.Fatal(err)
	}
	first, err := build(repo, stuckDataplane{newFake()}, envPool, 1)
	if err != nil {
		t.Fatal(err)
	}
	var cut atomic.Bool
	lrc := rest.CopyConfig(rc)
	lrc.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
		return roundTripper(func(r *http.Request) (*http.Response, error) {
			if !cut.Load() || r.Method != http.MethodPut || !strings.Contains(r.URL.Path, "/leases/") {
				return base.RoundTrip(r)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			if strings.Contains(string(body), `"holderIdentity":""`) || !strings.Contains(string(body), `"holderIdentity"`) {
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			return nil, errors.New("renewal connection cut")
		})
	}
	el, err := leaseElector(lrc, envNamespace, fastLease)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.launch(ctx, el); err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, first.Close)
	waitFor(t, first.leading.Load)
	second, _ := openReplica(t, kc)
	defer closeOrFail(t, second.Close)
	op, err := second.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.8.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		got, err := second.GetOperation(ctx, "proj", op.ID)
		return err == nil && got.State == domain.OpRunning
	})
	cut.Store(true)
	waitFor(t, second.leading.Load)
	if first.leading.Load() {
		t.Fatal("the old holder still runs workers after the next one took the Lease")
	}
	waitSucceeded(t, second, "proj", op)
}
