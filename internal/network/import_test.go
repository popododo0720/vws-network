package network

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func runImport(t *testing.T, src *sqliteRepo, dst repository, mode ImportMode) (string, error) {
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
	wantLine(t, report, "projects: 1, operations: 3, addresses: 1, differences: 0, missing: 4, writes: 4")
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
	if d, err := documentDiffs(upgradeFixtureProject, want, got); err != nil || len(d) > 0 {
		t.Fatalf("documents differ %v err %v", d, err)
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
	wantLine(t, report, "missing: 4, writes: 0")
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

// TestImportRefusesDifferentStore writes nothing when the store holds the
// project differently, and verify names the difference.
func TestImportRefusesDifferentStore(t *testing.T) {
	ctx := context.Background()
	src, _ := openFixtureCopy(t)
	dst := openKubeRepo(t, fake.NewClientBuilder().Build(), "vws-system")
	var other domain.State
	if err := dst.commit(ctx, commit{project: upgradeFixtureProject, state: &other}); err != nil {
		t.Fatal(err)
	}
	report, err := runImport(t, src, dst, ImportWrite)
	if !errors.Is(err, ErrImportDiffers) {
		t.Fatalf("import into a different store = %v\n%s", err, report)
	}
	wantLine(t, report, "differs: project a1001 vpc vpc-1: missing in the store")
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
