package network

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/popododo0720/vws-network/internal/domain"
)

const importFixture = "testdata/upgrade/b5d57ef/net.db"

// openFixtureCopy opens a copy of the released SQLite fixture, so the
// committed file is never written.
func openFixtureCopy(t *testing.T) (*sqliteRepo, string) {
	t.Helper()
	data, err := os.ReadFile(importFixture)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "net.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	repo, err := openSQLite(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeOrFail(t, repo.close) })
	return repo, path
}

func runImport(t *testing.T, src *sqliteRepo, dst *kubeRepo, mode ImportMode) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := importStore(context.Background(), src, dst, mode, &out)
	return out.String(), err
}

func wantLine(t *testing.T, report, line string) {
	t.Helper()
	if !strings.Contains(report, line) {
		t.Fatalf("report lacks %q:\n%s", line, report)
	}
}

// TestImportSQLiteFixture imports the released fixture: every record lands,
// verify passes, a second import writes nothing, and the pending operation is
// claimed from the new store.
func TestImportSQLiteFixture(t *testing.T) {
	ctx := context.Background()
	src, _ := openFixtureCopy(t)
	dst := openKubeRepo(t, fake.NewClientBuilder().Build(), "vws-system")

	report, err := runImport(t, src, dst, ImportWrite)
	if err != nil {
		t.Fatalf("import: %v\n%s", err, report)
	}
	wantLine(t, report, "projects: 1, operations: 3, addresses: 1, differences: 0, missing: 8, writes: 8")
	wantLine(t, report, "create: project a1001 operation op-pending key \"key-pending\"")
	if report, err := runImport(t, src, dst, ImportVerify); err != nil {
		t.Fatalf("verify: %v\n%s", err, report)
	}
	report, err = runImport(t, src, dst, ImportWrite)
	if err != nil {
		t.Fatalf("second import: %v\n%s", err, report)
	}
	wantLine(t, report, "differences: 0, missing: 0, writes: 0")

	want, _, err := src.load(ctx, upgradeFixtureProject)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := dst.load(ctx, upgradeFixtureProject)
	if err != nil {
		t.Fatal(err)
	}
	if steps, diffs, err := resourcePlan(upgradeFixtureProject, want, got); err != nil || len(steps)+len(diffs) > 0 {
		t.Fatalf("documents differ: %d missing, %v, err %v", len(steps), diffs, err)
	}
	j, ok, err := dst.claim(ctx, time.Now().Add(24*time.Hour), "after-import")
	if err != nil || !ok || j.op.ID != "op-pending" {
		t.Fatalf("claim %+v ok %v err %v", j.op, ok, err)
	}
}

// TestImportDryRunWritesNothing prints the planned objects and leaves the
// store empty.
func TestImportDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	src, _ := openFixtureCopy(t)
	dst := openKubeRepo(t, fake.NewClientBuilder().Build(), "vws-system")
	report, err := runImport(t, src, dst, ImportDryRun)
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, report)
	}
	wantLine(t, report, "would create: project a1001 vpc vpc-1")
	wantLine(t, report, "would create: project a1001 address ")
	wantLine(t, report, "missing: 8, writes: 0")
	if ids, err := dst.projectIDs(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("projects after dry run %v err %v", ids, err)
	}
	if report, err := runImport(t, src, dst, ImportVerify); !errors.Is(err, ErrImportDiffers) {
		t.Fatalf("verify of an empty store = %v\n%s", err, report)
	}
}

// TestImportResumesAfterCrash finishes an import that stopped after the
// project document: only the operations are written.
func TestImportResumesAfterCrash(t *testing.T) {
	ctx := context.Background()
	src, _ := openFixtureCopy(t)
	dst := openKubeRepo(t, fake.NewClientBuilder().Build(), "vws-system")
	st, _, err := src.load(ctx, upgradeFixtureProject)
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.commit(ctx, commit{project: upgradeFixtureProject, state: &st}); err != nil {
		t.Fatal(err)
	}
	report, err := runImport(t, src, dst, ImportWrite)
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, report)
	}
	wantLine(t, report, "missing: 3, writes: 3")
	if report, err := runImport(t, src, dst, ImportVerify); err != nil {
		t.Fatalf("verify: %v\n%s", err, report)
	}
}

// TestImportRefusesDifferentStore writes nothing when the store holds a
// resource differently, and verify names the difference.
func TestImportRefusesDifferentStore(t *testing.T) {
	ctx := context.Background()
	src, _ := openFixtureCopy(t)
	dst := openKubeRepo(t, fake.NewClientBuilder().Build(), "vws-system")
	st, _, err := src.load(ctx, upgradeFixtureProject)
	if err != nil {
		t.Fatal(err)
	}
	vpc := st.VPCs["vpc-1"]
	vpc.CIDR = mustP("10.99.0.0/16")
	other := domain.State{VPCs: map[domain.VPCID]domain.VPC{vpc.ID: vpc}}
	if err := dst.commit(ctx, commit{project: upgradeFixtureProject, state: &other}); err != nil {
		t.Fatal(err)
	}
	report, err := runImport(t, src, dst, ImportWrite)
	if !errors.Is(err, ErrImportDiffers) {
		t.Fatalf("import into a different store = %v\n%s", err, report)
	}
	wantLine(t, report, "differs: project a1001 vpc vpc-1: store has ")
	wantLine(t, report, "writes: 0")
	if _, ok, err := dst.key(ctx, upgradeFixtureProject, "key-pending"); err != nil || ok {
		t.Fatalf("operation written ok %v err %v", ok, err)
	}
	if _, err := runImport(t, src, dst, ImportVerify); !errors.Is(err, ErrImportDiffers) {
		t.Fatalf("verify = %v", err)
	}
}

// TestImportVerifyFindsChangedOperation fails verify after an operation moved
// in the store.
func TestImportVerifyFindsChangedOperation(t *testing.T) {
	ctx := context.Background()
	src, _ := openFixtureCopy(t)
	dst := openKubeRepo(t, fake.NewClientBuilder().Build(), "vws-system")
	if report, err := runImport(t, src, dst, ImportWrite); err != nil {
		t.Fatalf("import: %v\n%s", err, report)
	}
	if _, ok, err := dst.claim(ctx, time.Now().Add(24*time.Hour), "moved"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	report, err := runImport(t, src, dst, ImportVerify)
	if !errors.Is(err, ErrImportDiffers) {
		t.Fatalf("verify = %v\n%s", err, report)
	}
	wantLine(t, report, "differs: project a1001 key \"key-pending\"")
}

func TestImportSQLiteRefusesBadInput(t *testing.T) {
	ctx := context.Background()
	store := StoreConfig{Namespace: "vws-system"}
	var out bytes.Buffer
	if err := ImportSQLite(ctx, "", store, ImportWrite, &out); kind(err) != domain.Invalid {
		t.Fatalf("empty file = %v", err)
	}
	missing := filepath.Join(t.TempDir(), "absent.db")
	if err := ImportSQLite(ctx, missing, store, ImportWrite, &out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file = %v", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("import created the missing file: %v", err)
	}
	_, path := openFixtureCopy(t)
	if err := ImportSQLite(ctx, path, StoreConfig{Namespace: "Bad_NS"}, ImportWrite, &out); kind(err) != domain.Invalid {
		t.Fatalf("bad namespace = %v", err)
	}
}

// TestImportReadOnlyLeavesUnfinishedChange: dry run and verify do not finish
// a change a crash left in a head; they report it, and a write run finishes it.
func TestImportReadOnlyLeavesUnfinishedChange(t *testing.T) {
	ctx := context.Background()
	src, _ := openFixtureCopy(t)
	c := fake.NewClientBuilder().Build()
	dst := openKubeRepo(t, c, "vws-system")
	if report, err := runImport(t, src, dst, ImportWrite); err != nil {
		t.Fatalf("import: %v\n%s", err, report)
	}
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: "vws-system", Name: headName(upgradeFixtureProject)}
	if err := c.Get(ctx, key, &cm); err != nil {
		t.Fatal(err)
	}
	delete(cm.Data, "applied")
	if err := c.Update(ctx, &cm); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, &cm); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []ImportMode{ImportDryRun, ImportVerify} {
		report, err := runImport(t, src, &kubeRepo{c: c, ns: "vws-system", now: time.Now}, mode)
		if !errors.Is(err, ErrImportDiffers) {
			t.Fatalf("mode %d = %v\n%s", mode, err, report)
		}
		wantLine(t, report, "differs: project a1001: the store has an unfinished change")
		var after corev1.ConfigMap
		if err := c.Get(ctx, key, &after); err != nil {
			t.Fatal(err)
		}
		if after.ResourceVersion != cm.ResourceVersion {
			t.Fatalf("mode %d wrote the head", mode)
		}
	}
	report, err := runImport(t, src, openKubeRepo(t, c, "vws-system"), ImportWrite)
	if err != nil {
		t.Fatalf("finishing import: %v\n%s", err, report)
	}
	wantLine(t, report, "differences: 0, missing: 0, writes: 0")
}

// TestImportVerifyFindsExtraRecords fails verify on a project, operation,
// key or address the store holds and the file does not.
func TestImportVerifyFindsExtraRecords(t *testing.T) {
	ctx := context.Background()
	src, _ := openFixtureCopy(t)
	dst := openKubeRepo(t, fake.NewClientBuilder().Build(), "vws-system")
	if report, err := runImport(t, src, dst, ImportWrite); err != nil {
		t.Fatalf("import: %v\n%s", err, report)
	}
	extra := job{op: domain.Operation{
		ID: "op-extra", Project: upgradeFixtureProject, Type: domain.OpCreateVPC, Resource: "vpc-extra",
		State: domain.OpSucceeded, Key: "key-extra",
	}}
	if err := dst.commit(ctx, commit{project: upgradeFixtureProject, enqueue: &extra}); err != nil {
		t.Fatal(err)
	}
	var empty domain.State
	if err := dst.commit(ctx, commit{project: "a2002", state: &empty}); err != nil {
		t.Fatal(err)
	}
	report, err := runImport(t, src, dst, ImportVerify)
	if !errors.Is(err, ErrImportDiffers) {
		t.Fatalf("verify = %v\n%s", err, report)
	}
	wantLine(t, report, "differs: project a1001 operation op-extra: only in the store")
	wantLine(t, report, "differs: project a1001 key \"key-extra\": only in the store")
	wantLine(t, report, "differs: project a2002: only in the store")
}

// TestImportLargeProject imports a project whose documents together exceed
// one object: each resource is its own commit.
func TestImportLargeProject(t *testing.T) {
	ctx := context.Background()
	src, err := openSQLite(ctx, filepath.Join(t.TempDir(), "net.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeOrFail(t, src.close) })
	svc, _ := openTest(t, src)
	text := widestText()
	vpcOp, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.0.0.0/16"), Text: text})
	if err != nil {
		t.Fatal(err)
	}
	if err := drain(svc); err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		_, err := svc.CreateSubnet(ctx, call("proj", fmt.Sprintf("sub-%d", i)), domain.CreateSubnet{
			VPC: domain.VPCID(vpcOp.Resource), CIDR: mustP(fmt.Sprintf("10.0.%d.0/24", i)), Zone: "az1", Text: text,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := drain(svc); err != nil {
			t.Fatal(err)
		}
	}
	c := fake.NewClientBuilder().Build()
	dst := openKubeRepo(t, c, "vws-system")
	if report, err := runImport(t, src, dst, ImportWrite); err != nil {
		t.Fatalf("import: %v\n%s", err, report)
	}
	if report, err := runImport(t, src, dst, ImportVerify); err != nil {
		t.Fatalf("verify: %v\n%s", err, report)
	}
	largestObjects(t, c)
}

// TestImportRefusesBeforeWriting finds what the store would refuse while it
// plans, so a refused import writes nothing at all.
func TestImportRefusesBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, src *sqliteRepo, dst *kubeRepo)
		want  string
	}{
		{"id in two projects of the file", func(t *testing.T, src *sqliteRepo, _ *kubeRepo) {
			t.Helper()
			ctx := context.Background()
			if _, err := src.db.ExecContext(ctx,
				`INSERT INTO projects (id, version, state) SELECT 'a2002', version, state FROM projects`); err != nil {
				t.Fatal(err)
			}
		}, "differs: vpc vpc-1: held by projects a1001 and a2002 in the file"},
		{"address of another project in the store", func(t *testing.T, src *sqliteRepo, dst *kubeRepo) {
			t.Helper()
			ctx := context.Background()
			st, _, err := src.load(ctx, upgradeFixtureProject)
			if err != nil {
				t.Fatal(err)
			}
			eip := st.ElasticIPs["eipalloc-1"]
			eip.ID = "eipalloc-other"
			other := domain.State{ElasticIPs: map[domain.ElasticIPID]domain.ElasticIP{eip.ID: eip}}
			if err := dst.commit(ctx, commit{project: "a2002", state: &other}); err != nil {
				t.Fatal(err)
			}
		}, "held by project a2002 in the store"},
		{"document too large for the store", func(t *testing.T, src *sqliteRepo, _ *kubeRepo) {
			t.Helper()
			ctx := context.Background()
			st, _, err := src.load(ctx, upgradeFixtureProject)
			if err != nil {
				t.Fatal(err)
			}
			vpc := st.VPCs["vpc-1"]
			vpc.Description = strings.Repeat("x", 200<<10)
			st.VPCs[vpc.ID] = vpc
			blob, err := domain.MarshalState(st)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := src.db.ExecContext(ctx, `UPDATE projects SET state = ?`, blob); err != nil {
				t.Fatal(err)
			}
		}, "differs: project a1001 vpc vpc-1: too large for the store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, _ := openFixtureCopy(t)
			dst := openKubeRepo(t, fake.NewClientBuilder().Build(), "vws-system")
			tc.setup(t, src, dst)
			report, err := runImport(t, src, dst, ImportWrite)
			if !errors.Is(err, ErrImportDiffers) {
				t.Fatalf("import = %v\n%s", err, report)
			}
			wantLine(t, report, tc.want)
			wantLine(t, report, "writes: 0")
			if _, ok, err := dst.key(context.Background(), upgradeFixtureProject, "key-pending"); err != nil || ok {
				t.Fatalf("operation written ok %v err %v", ok, err)
			}
		})
	}
}

// TestSQLiteCurrentRefusesOldSchema keeps a dry run or verify from migrating
// the file: an older schema is refused and the file is left as it was.
func TestSQLiteCurrentRefusesOldSchema(t *testing.T) {
	ctx := context.Background()
	src, path := openFixtureCopy(t)
	if err := sqliteCurrent(ctx, path); err != nil {
		t.Fatalf("current schema = %v", err)
	}
	if _, err := src.db.ExecContext(ctx, `PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := sqliteCurrent(ctx, path); kind(err) != domain.Invalid {
		t.Fatalf("old schema = %v", err)
	}
	var version int
	if err := src.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("user_version %d err %v, want 1", version, err)
	}
}

type importCrash struct {
	client.Client
	remaining int
}

func (c *importCrash) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if c.remaining == 0 {
		return errInjected
	}
	c.remaining--
	return c.Client.Create(ctx, obj, opts...)
}

func (c *importCrash) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if c.remaining == 0 {
		return errInjected
	}
	c.remaining--
	return c.Client.Update(ctx, obj, opts...)
}

func TestImportRestartsAtEveryWrite(t *testing.T) {
	baseline, _ := openFixtureCopy(t)
	counter := &importCrash{Client: fake.NewClientBuilder().Build(), remaining: 1 << 30}
	if _, err := runImport(t, baseline, openKubeRepo(t, counter, "vws-system"), ImportWrite); err != nil {
		t.Fatal(err)
	}
	writes := (1 << 30) - counter.remaining
	if writes == 0 {
		t.Fatal("fixture import made no writes")
	}
	t.Logf("interrupting all %d destination writes", writes)
	for cut := 0; cut <= writes; cut++ {
		t.Run(fmt.Sprint(cut), func(t *testing.T) {
			ctx := context.Background()
			src, path := openFixtureCopy(t)
			base := fake.NewClientBuilder().Build()
			broken := openKubeRepo(t, &importCrash{Client: base, remaining: cut}, "vws-system")
			_, interrupted := runImport(t, src, broken, ImportWrite)
			if cut < writes && !errors.Is(interrupted, errInjected) {
				t.Fatalf("expected interruption after %d writes: %v", cut, interrupted)
			}
			if cut == writes && interrupted != nil {
				t.Fatalf("complete import: %v", interrupted)
			}
			closeOrFail(t, src.close)
			reopened, err := openSQLite(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { closeOrFail(t, reopened.close) })
			restarted := openKubeRepo(t, base, "vws-system")
			if report, err := runImport(t, reopened, restarted, ImportWrite); err != nil {
				t.Fatalf("restart: %v\n%s", err, report)
			}
			if report, err := runImport(t, reopened, restarted, ImportVerify); err != nil {
				t.Fatalf("restart lost or duplicated records: %v\n%s", err, report)
			}
			report, err := runImport(t, reopened, restarted, ImportWrite)
			if err != nil {
				t.Fatal(err)
			}
			wantLine(t, report, "differences: 0, missing: 0, writes: 0")
		})
	}
}
