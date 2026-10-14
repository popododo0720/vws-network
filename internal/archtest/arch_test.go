// Package archtest enforces this module's layer boundaries on the imports and
// selectors of every non-test Go file. Modeled on vws-compute internal/archtest.
package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/popododo0720/vws-network"

func isModule(imp string) bool { return imp == module || strings.HasPrefix(imp, module+"/") }

func under(dir string, roots ...string) bool {
	for _, r := range roots {
		if dir == r || strings.HasPrefix(dir, r+"/") {
			return true
		}
	}
	return false
}

func hasPrefix(prefixes ...string) func(string) bool {
	return func(s string) bool { return under(s, prefixes...) }
}

var (
	isK8s   = hasPrefix("k8s.io", "sigs.k8s.io")
	isSQL   = hasPrefix("database/sql", "modernc.org/sqlite")
	isGRPC  = hasPrefix("google.golang.org/grpc")
	isWire  = hasPrefix("google.golang.org/protobuf", "google.golang.org/genproto", "github.com/popododo0720/vws-proto", "cloud.google.com/go/longrunning")
	isObsLb = hasPrefix("github.com/popododo0720/vws-lib")
)

// packageRules forbid imports inside one package directory.
var packageRules = []struct {
	dir    string
	forbid func(imp string) bool
	why    string
}{
	{"internal/domain", func(imp string) bool {
		return isModule(imp) || isK8s(imp) || isSQL(imp) || isGRPC(imp) || isWire(imp) || isObsLb(imp) || imp == "log/slog" || imp == "context"
	}, "the domain is pure: no module package, Kubernetes, storage, transport, or logging"},
	{"internal/network", hasPrefix(module + "/internal/grpcapi"), "the service never sees the gRPC edge"},
	{"internal/grpcapi", hasPrefix(module + "/cmd"), "handlers do not reach the binary"},
}

// ownerRules confine an import to the directories that own it.
var ownerRules = []struct {
	match  func(imp string) bool
	owners []string
	why    string
}{
	{isK8s, []string{"internal/network"}, "Kubernetes client types stay in the Kube-OVN dataplane"},
	{isSQL, []string{"internal/network"}, "SQL stays in the repository"},
	{isWire, []string{"internal/grpcapi"}, "wire types stay in grpcapi"},
	{isGRPC, []string{"internal/grpcapi", "cmd/vws-network"}, "gRPC stays at the edge"},
	{func(imp string) bool { return imp == "log" }, nil, "no std log: slog through vws-lib obs"},
	{hasPrefix("github.com/getsentry/sentry-go"), nil, "no direct sentry: vws-lib obs/report"},
	{func(imp string) bool { return imp == "testing" }, nil, "production code does not import testing"},
}

// qualifiedRules confine a package-qualified identifier (pkg.Name) to the
// directories allowed to use it.
var qualifiedRules = []struct {
	pkg, name string
	owners    []string
	why       string
}{
	{"domain", "State", []string{"internal/domain", "internal/network"}, "resource rules run only inside network.Service"},
}

// constrainedDirs must exist: renaming one would silently disable its rule.
func constrainedDirs() []string {
	dirs := []string{"cmd/vws-network"}
	for _, r := range packageRules {
		dirs = append(dirs, r.dir)
	}
	for _, r := range ownerRules {
		dirs = append(dirs, r.owners...)
	}
	return dirs
}

func archViolations(pkgs map[string][]*ast.File) []string {
	var bad []string
	for dir, files := range pkgs {
		for _, f := range files {
			for _, spec := range f.Imports {
				imp, _ := strconv.Unquote(spec.Path.Value)
				for _, r := range packageRules {
					if dir == r.dir && r.forbid(imp) {
						bad = append(bad, fmt.Sprintf("%s imports %s: %s", dir, imp, r.why))
					}
				}
				for _, r := range ownerRules {
					if r.match(imp) && !under(dir, r.owners...) {
						bad = append(bad, fmt.Sprintf("%s imports %s: %s", dir, imp, r.why))
					}
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				x, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				for _, r := range qualifiedRules {
					if x.Name == r.pkg && sel.Sel.Name == r.name && !under(dir, r.owners...) {
						bad = append(bad, fmt.Sprintf("%s uses %s.%s: %s", dir, r.pkg, r.name, r.why))
					}
				}
				return true
			})
		}
	}
	sort.Strings(bad)
	return bad
}

func walkFiles(root string, visit func(rel string, data []byte)) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == ".git" || n == "third_party" || n == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		visit(filepath.ToSlash(rel), data)
		return nil
	})
}

func loadProduction(t *testing.T, root string) map[string][]*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	pkgs := map[string][]*ast.File{}
	err := walkFiles(root, func(rel string, data []byte) {
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return
		}
		f, err := parser.ParseFile(fset, rel, data, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		pkgs[dir] = append(pkgs[dir], f)
	})
	if err != nil {
		t.Fatal(err)
	}
	return pkgs
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestArchitectureRules(t *testing.T) {
	probe := t.TempDir()
	for rel, src := range map[string]string{
		"internal/domain/d.go":           `package domain; import _ "log/slog"; import _ "` + module + `/internal/network"`,
		"internal/network/n.go":          `package network; import _ "` + module + `/internal/grpcapi"; import _ "google.golang.org/grpc"`,
		"internal/network/ok.go":         `package network; import _ "k8s.io/apimachinery/pkg/types"; import _ "modernc.org/sqlite"; var _ domain.State`,
		"internal/grpcapi/g.go":          `package grpcapi; import _ "k8s.io/client-go/rest"; import _ "database/sql"; func f(op O) { _ = op.State; _ = domain.State{} }`,
		"internal/grpcapi/ok.go":         `package grpcapi; import _ "google.golang.org/grpc"; import _ "google.golang.org/protobuf/proto"`,
		"cmd/vws-network/m.go":           `package main; import _ "log"; import _ "github.com/getsentry/sentry-go"; import _ "testing"`,
		"internal/network/n_test.go":     `package network; import _ "testing"; import _ "` + module + `/internal/grpcapi"`,
		"internal/network/testdata/x.go": `package x; import _ "log"`,
	} {
		writeFile(t, filepath.Join(probe, rel), src)
	}
	want := []string{
		"cmd/vws-network imports github.com/getsentry/sentry-go: no direct sentry: vws-lib obs/report",
		"cmd/vws-network imports log: no std log: slog through vws-lib obs",
		"cmd/vws-network imports testing: production code does not import testing",
		"internal/domain imports " + module + "/internal/network: the domain is pure: no module package, Kubernetes, storage, transport, or logging",
		"internal/domain imports log/slog: the domain is pure: no module package, Kubernetes, storage, transport, or logging",
		"internal/grpcapi imports database/sql: SQL stays in the repository",
		"internal/grpcapi imports k8s.io/client-go/rest: Kubernetes client types stay in the Kube-OVN dataplane",
		"internal/grpcapi uses domain.State: resource rules run only inside network.Service",
		"internal/network imports " + module + "/internal/grpcapi: the service never sees the gRPC edge",
		"internal/network imports google.golang.org/grpc: gRPC stays at the edge",
	}
	if got := archViolations(loadProduction(t, probe)); !reflect.DeepEqual(got, want) {
		t.Fatalf("probe violations:\n got %q\nwant %q", got, want)
	}

	root := repoRoot(t)
	for _, dir := range constrainedDirs() {
		if _, err := os.Stat(filepath.Join(root, dir)); err != nil {
			t.Errorf("rule directory %s: %v", dir, err)
		}
	}
	for _, v := range archViolations(loadProduction(t, root)) {
		t.Error(v)
	}
}
