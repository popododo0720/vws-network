package standards

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestVWSProtoIsATag: vws-proto comes from a release tag, with no replace, no copy and no generated code here.
func TestVWSProtoIsATag(t *testing.T) {
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, "proto")); !os.IsNotExist(err) {
		t.Fatal("proto sources belong in vws-proto")
	}
	if _, err := os.Stat(filepath.Join(root, "gen")); !os.IsNotExist(err) {
		t.Fatal("generated code belongs in vws-proto")
	}
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	failures := protoModuleRules(string(b))
	if _, err := os.Stat(filepath.Join(root, "third_party", "vws-proto")); !os.IsNotExist(err) {
		failures = append(failures, "vendored copy third_party/vws-proto; require a vws-proto tag in go.mod instead")
	}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".pb.go") {
			failures = append(failures, "generated protobuf "+path+"; import it from the vws-proto module")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) > 0 {
		t.Fatalf("vws-proto module rules:\n%s", strings.Join(failures, "\n"))
	}
}

// protoModulePath is the resource API module. It is consumed from a release tag through the module proxy.
const protoModulePath = "github.com/popododo0720/vws-proto"

// protoRequire matches the require line for protoModulePath at a release tag (no pseudo-version, no pre-release).
var protoRequire = regexp.MustCompile(
	`^(require\s+)?` + regexp.QuoteMeta(protoModulePath) + `\s+v[0-9]+\.[0-9]+\.[0-9]+$`,
)

// protoModuleRules returns the go.mod problems for the resource API module: a replace directive
// (a local copy), or a require that is missing or not a release tag.
func protoModuleRules(mod string) []string {
	var failures []string
	tagged := false
	for _, raw := range strings.Split(mod, "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if strings.Contains(line, "=>") && strings.Contains(line, protoModulePath) {
			failures = append(failures, "go.mod replaces "+protoModulePath+" ("+line+"); require a release tag instead")
		}
		if protoRequire.MatchString(line) {
			tagged = true
		}
	}
	if !tagged {
		failures = append(failures, "go.mod does not require "+protoModulePath+" at a release tag vX.Y.Z")
	}
	return failures
}

func TestProtoModuleRules(t *testing.T) {
	const tagged = "require github.com/popododo0720/vws-proto v0.5.0\n"
	cases := []struct {
		name string
		mod  string
		bad  bool
	}{
		{"tag in a require block", "require (\n\tgithub.com/popododo0720/vws-proto v0.5.0\n)\n", false},
		{"single-line require", tagged, false},
		{"tag with indirect comment", "require (\n\tgithub.com/popododo0720/vws-proto v0.5.0 // indirect\n)\n", false},
		{"replace to a local copy", tagged + "replace github.com/popododo0720/vws-proto => ./third_party/vws-proto\n", true},
		{"replace in a block", tagged + "replace (\n\tgithub.com/popododo0720/vws-proto => ../vws-proto\n)\n", true},
		{"pseudo-version", "require github.com/popododo0720/vws-proto v0.4.1-0.20261021123535-8f0a84ed9918\n", true},
		{"placeholder version", "require github.com/popododo0720/vws-proto v0.0.0-00010101000000-000000000000\n", true},
		{"pre-release", "require github.com/popododo0720/vws-proto v0.6.0-rc.1\n", true},
		{"missing", "require google.golang.org/grpc v1.84.0\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := protoModuleRules(tc.mod)
			if (len(got) > 0) != tc.bad {
				t.Fatalf("protoModuleRules(%q) = %q, want failure %v", tc.mod, got, tc.bad)
			}
		})
	}
}

func TestNoDeployment(t *testing.T) {
	root := filepath.Join("..", "..")
	banned := map[string]bool{
		"dockerfile":         true,
		"chart.yaml":         true,
		"kustomization.yaml": true,
	}
	bannedDir := map[string]bool{"deploy": true, "k8s": true, "helm": true, "charts": true}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		base := strings.ToLower(d.Name())
		if d.IsDir() && (base == ".git" || base == "gen") {
			return filepath.SkipDir
		}
		if d.IsDir() && bannedDir[base] {
			t.Fatalf("deployment directory %s", path)
		}
		if banned[base] {
			t.Fatalf("deployment file %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNoBrandNotice(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		base := strings.ToLower(d.Name())
		if d.IsDir() && (base == ".git" || base == "gen") {
			return filepath.SkipDir
		}
		if d.IsDir() || base == "license" {
			return nil
		}
		if !strings.HasSuffix(base, ".md") && !strings.HasSuffix(base, ".go") && !strings.HasSuffix(base, ".proto") && !strings.HasSuffix(base, ".yml") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lower := strings.ToLower(string(b))
		brand := "trade" + "mark"
		rights := "all rights " + "reserved"
		if strings.Contains(lower, brand) || strings.Contains(lower, rights) {
			t.Fatalf("%s contains a brand or rights notice", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
