//go:build envtest

package network

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/popododo0720/vws-network/internal/domain"
)

// TestEnvtestImportSQLiteFixture runs the import of the released fixture
// against a real API server through ImportSQLite: a dry run leaves the object
// count unchanged, the import passes -verify, a second import writes nothing,
// and the project reads back equal to the file. It runs as a user with only
// the configmaps rule of the store Role.
func TestEnvtestImportSQLiteFixture(t *testing.T) {
	env, admin := startEnv(t)
	ctx := context.Background()
	b, err := roleUser(t, env, admin, "vws-network-import", storeRules[:1]).KubeConfig()
	if err != nil {
		t.Fatal(err)
	}
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kc, b, 0o600); err != nil {
		t.Fatal(err)
	}
	store := StoreConfig{Namespace: envNamespace, Kubeconfig: kc}
	src, path := openFixtureCopy(t)

	before := storeObjects(t, admin)
	var out bytes.Buffer
	if err := ImportSQLite(ctx, path, store, ImportDryRun, &out); err != nil {
		t.Fatalf("dry run: %v\n%s", err, out.String())
	}
	if after := storeObjects(t, admin); !reflect.DeepEqual(before, after) {
		t.Fatalf("dry run wrote objects: %v, before %v", after, before)
	}
	t.Log(out.String())
	for _, step := range []struct {
		mode ImportMode
		want string
	}{
		{ImportWrite, "missing: 8, writes: 8"},
		{ImportVerify, "differences: 0, missing: 0, writes: 0"},
		{ImportWrite, "differences: 0, missing: 0, writes: 0"},
	} {
		out.Reset()
		if err := ImportSQLite(ctx, path, store, step.mode, &out); err != nil {
			t.Fatalf("mode %d: %v\n%s", step.mode, err, out.String())
		}
		if !strings.Contains(out.String(), step.want) {
			t.Fatalf("mode %d report lacks %q:\n%s", step.mode, step.want, out.String())
		}
		t.Log(out.String())
	}
	imported := storeObjects(t, admin)
	out.Reset()
	if err := ImportSQLite(ctx, path, store, ImportDryRun, &out); err != nil {
		t.Fatalf("dry run after import: %v\n%s", err, out.String())
	}
	if after := storeObjects(t, admin); !reflect.DeepEqual(imported, after) {
		t.Fatalf("dry run after import changed objects: %v, before %v", after, imported)
	}
	want, _, err := src.load(ctx, upgradeFixtureProject)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := openKubeRepo(t, admin, envNamespace).load(ctx, upgradeFixtureProject)
	if err != nil {
		t.Fatal(err)
	}
	if steps, diffs, err := resourcePlan(upgradeFixtureProject, want, got); err != nil || len(steps)+len(diffs) > 0 {
		t.Fatalf("documents differ: %d missing, %v, err %v", len(steps), diffs, err)
	}
}

func TestEnvtestImportLifecycle(t *testing.T) {
	env, admin := startEnv(t)
	ctx := context.Background()
	store := StoreConfig{Namespace: envNamespace, Kubeconfig: kubeconfigFor(t, env)}
	src, path := openFixtureCopy(t)
	dst := &kubeRepo{c: admin, ns: envNamespace}
	digest, err := importSource(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admitLifecycle(ctx, dst, lifecycleImporting, digest); err != nil {
		t.Fatal(err)
	}
	before := storeObjects(t, admin)
	if _, _, err := openStore(ctx, Config{Store: store}); kind(err) != domain.Conflict {
		t.Fatalf("service entered unfinished import = %v", err)
	}
	if after := storeObjects(t, admin); !reflect.DeepEqual(before, after) {
		t.Fatal("blocked service repaired or changed the store")
	}
	if err := ImportSQLite(ctx, path, store, ImportWrite, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := ImportSQLite(ctx, path, store, ImportVerify, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openStore(ctx, Config{Store: store}); err != nil {
		t.Fatalf("service after import = %v", err)
	}
	if _, err := src.db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	before = storeObjects(t, admin)
	if err := ImportSQLite(ctx, path, store, ImportWrite, io.Discard); err != nil {
		t.Fatal(err)
	}
	if after := storeObjects(t, admin); !reflect.DeepEqual(before, after) {
		t.Fatal("completed import wrote destination records")
	}
	var version int
	if err := src.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("no-op migrated source: %d, %v", version, err)
	}
}

func TestEnvtestImportRefusesServedStore(t *testing.T) {
	env, admin := startEnv(t)
	ctx := context.Background()
	store := StoreConfig{Namespace: envNamespace, Kubeconfig: kubeconfigFor(t, env)}
	if _, _, err := openStore(ctx, Config{Store: store}); err != nil {
		t.Fatal(err)
	}
	src, path := openFixtureCopy(t)
	if _, err := src.db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	before := storeObjects(t, admin)
	if err := ImportSQLite(ctx, path, store, ImportWrite, io.Discard); kind(err) != domain.Conflict {
		t.Fatalf("import after service start = %v", err)
	}
	if after := storeObjects(t, admin); !reflect.DeepEqual(before, after) {
		t.Fatal("refused import wrote destination records")
	}
	var version int
	if err := src.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("refused import migrated source: %d, %v", version, err)
	}
}
