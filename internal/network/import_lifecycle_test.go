package network

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/popododo0720/vws-network/internal/domain"
)

func TestImportLifecycleServiceFirst(t *testing.T) {
	ctx := context.Background()
	src, _ := openFixtureCopy(t)
	base := fake.NewClientBuilder().Build()
	dst := openKubeRepo(t, base, "vws-system")
	if _, err := admitLifecycle(ctx, dst, lifecycleServing, ""); err != nil {
		t.Fatal(err)
	}
	restarted := &kubeRepo{c: &importCrash{Client: base}, ns: dst.ns}
	if _, err := runImport(t, src, restarted, ImportWrite); kind(err) != domain.Conflict {
		t.Fatalf("import after service start = %v", err)
	}
	if _, err := admitLifecycle(ctx, restarted, lifecycleServing, ""); err != nil {
		t.Fatalf("service restart = %v", err)
	}
	if ids, err := dst.projectIDs(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("import wrote projects %v: %v", ids, err)
	}
}

func TestImportLifecycleSnapshotBinding(t *testing.T) {
	ctx := context.Background()
	src, _ := openFixtureCopy(t)
	dst := openKubeRepo(t, fake.NewClientBuilder().Build(), "vws-system")
	digest, err := importSource(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admitLifecycle(ctx, dst, lifecycleImporting, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := admitLifecycle(ctx, dst, lifecycleServing, ""); kind(err) != domain.Conflict {
		t.Fatalf("service during import = %v", err)
	}
	other, _ := openFixtureCopy(t)
	if _, err := other.db.ExecContext(ctx, "DELETE FROM operations WHERE id = 'op-pending'"); err != nil {
		t.Fatal(err)
	}
	if _, err := runImport(t, other, dst, ImportWrite); kind(err) != domain.Conflict {
		t.Fatalf("different source resumed import = %v", err)
	}
	if _, err := runImport(t, src, dst, ImportWrite); err != nil {
		t.Fatalf("same source restart = %v", err)
	}
	if _, err := admitLifecycle(ctx, dst, lifecycleServing, ""); err != nil {
		t.Fatalf("service after import = %v", err)
	}
}

func TestImportLifecycleCompletedIsNoOp(t *testing.T) {
	ctx := context.Background()
	src, _ := openFixtureCopy(t)
	base := fake.NewClientBuilder().Build()
	dst := openKubeRepo(t, base, "vws-system")
	if _, err := runImport(t, src, dst, ImportWrite); err != nil {
		t.Fatal(err)
	}
	if _, err := admitLifecycle(ctx, dst, lifecycleServing, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := dst.claim(ctx, dst.now().AddDate(0, 0, 1), "served"); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	// A completed import must not restore the old pending operation or repair
	// destination records, even if the source is no longer readable.
	if err := src.db.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := &kubeRepo{c: &importCrash{Client: base}, ns: dst.ns}
	report, err := runImport(t, src, restarted, ImportWrite)
	if err != nil {
		t.Fatal(err)
	}
	wantLine(t, report, "already imported; differences: 0, missing: 0, writes: 0")
}

func TestImportLifecycleAdmissionRace(t *testing.T) {
	for range 40 {
		dst := &kubeRepo{c: fake.NewClientBuilder().Build(), ns: "vws-system"}
		start := make(chan struct{})
		result := make(chan error, 2)
		for _, state := range []lifecycleState{lifecycleServing, lifecycleImporting} {
			go func() {
				<-start
				source := ""
				if state == lifecycleImporting {
					source = strings.Repeat("a", 64)
				}
				_, err := admitLifecycle(context.Background(), dst, state, source)
				result <- err
			}()
		}
		close(start)
		a, b := <-result, <-result
		if (a == nil) == (b == nil) {
			t.Fatalf("admission winners: %v, %v", a, b)
		}
		if a != nil && kind(a) != domain.Conflict || b != nil && kind(b) != domain.Conflict {
			t.Fatalf("admission error: %v, %v", a, b)
		}
	}
}

func TestImportLifecycleFailsClosed(t *testing.T) {
	for _, data := range []map[string]string{
		{},
		{"state": "unknown"},
		{"state": "importing"},
		{"state": "imported", "source": strings.Repeat("z", 64)},
	} {
		c := fake.NewClientBuilder().WithObjects(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: lifecycleName, Namespace: "vws-system"}, Data: data,
		}).Build()
		dst := &kubeRepo{c: c, ns: "vws-system"}
		if _, err := admitLifecycle(context.Background(), dst, lifecycleServing, ""); kind(err) != domain.Conflict {
			t.Fatalf("corrupt marker %v allowed service: %v", data, err)
		}
		if _, err := importFinished(context.Background(), dst); kind(err) != domain.Conflict {
			t.Fatalf("corrupt marker %v allowed import: %v", data, err)
		}
	}
	c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errInjected
		},
	}).Build()
	dst := &kubeRepo{c: c, ns: "vws-system"}
	if _, err := admitLifecycle(context.Background(), dst, lifecycleServing, ""); !errors.Is(err, errInjected) {
		t.Fatalf("API failure allowed service: %v", err)
	}
	if _, err := importFinished(context.Background(), dst); !errors.Is(err, errInjected) {
		t.Fatalf("API failure allowed import: %v", err)
	}
}

type importReportFailure struct{}

func (importReportFailure) Write([]byte) (int, error) { return 0, errInjected }

func TestImportLifecycleReportFailureBlocksServing(t *testing.T) {
	ctx := context.Background()
	src, _ := openFixtureCopy(t)
	dst := openKubeRepo(t, fake.NewClientBuilder().Build(), "vws-system")
	if err := importStore(ctx, src, dst, ImportWrite, importReportFailure{}); !errors.Is(err, errInjected) {
		t.Fatalf("report failure = %v", err)
	}
	if _, err := admitLifecycle(ctx, dst, lifecycleServing, ""); kind(err) != domain.Conflict {
		t.Fatalf("service after incomplete report = %v", err)
	}
	if err := importStore(ctx, src, dst, ImportWrite, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := admitLifecycle(ctx, dst, lifecycleServing, ""); err != nil {
		t.Fatal(err)
	}
	if err := importStore(ctx, src, dst, ImportWrite, importReportFailure{}); !errors.Is(err, errInjected) {
		t.Fatalf("no-op report failure = %v", err)
	}
}

func TestImportLifecycleReadOnlyDoesNotClaimStore(t *testing.T) {
	src, _ := openFixtureCopy(t)
	base := fake.NewClientBuilder().Build()
	dst := &kubeRepo{c: &importCrash{Client: base}, ns: "vws-system"}
	for _, mode := range []ImportMode{ImportDryRun, ImportVerify} {
		var out bytes.Buffer
		err := importStore(context.Background(), src, dst, mode, &out)
		if mode == ImportDryRun && err != nil || mode == ImportVerify && !errors.Is(err, ErrImportDiffers) {
			t.Fatal(err)
		}
	}
	if cm, err := dst.get(context.Background(), lifecycleName); err != nil || cm != nil {
		t.Fatalf("read-only import created marker: %v, %v", cm, err)
	}
	err := ImportSQLite(context.Background(), "", StoreConfig{}, ImportMode(99), io.Discard)
	if kind(err) != domain.Invalid {
		t.Fatalf("unknown mode = %v", err)
	}
}

func TestImportLifecycleStaleImporterCannotOverwriteServedState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	src, _ := openFixtureCopy(t)
	base := fake.NewClientBuilder().Build()
	blocked, resume := make(chan struct{}), make(chan struct{})
	defer close(resume)
	var once sync.Once
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetName() == headName(upgradeFixtureProject) {
				once.Do(func() {
					close(blocked)
					select {
					case <-resume:
					case <-ctx.Done():
					}
				})
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	result := make(chan error, 1)
	go func() { result <- importStore(ctx, src, &kubeRepo{c: c, ns: "vws-system"}, ImportWrite, io.Discard) }()
	select {
	case <-blocked:
	case err := <-result:
		t.Fatalf("import did not reach first project write: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	dst := openKubeRepo(t, base, "vws-system")
	if err := importStore(ctx, src, dst, ImportWrite, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := admitLifecycle(ctx, dst, lifecycleServing, ""); err != nil {
		t.Fatal(err)
	}
	st, version, err := dst.load(ctx, upgradeFixtureProject)
	if err != nil {
		t.Fatal(err)
	}
	vpc := st.VPCs["vpc-1"]
	vpc.Description = "changed after serving"
	st.VPCs[vpc.ID] = vpc
	if err := dst.commit(ctx, commit{project: upgradeFixtureProject, base: version, state: &st}); err != nil {
		t.Fatal(err)
	}
	resume <- struct{}{}
	if err := <-result; !errors.Is(err, errStale) {
		t.Fatalf("stale importer = %v", err)
	}
	after, _, err := dst.load(ctx, upgradeFixtureProject)
	if err != nil || after.VPCs[vpc.ID].Description != vpc.Description {
		t.Fatalf("stale import overwrote served data: %v, %v", after.VPCs[vpc.ID], err)
	}
}
