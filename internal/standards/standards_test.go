package standards

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProtoIsTemporary(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "proto", "vws", "network", "v1", "network.proto"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !strings.Contains(text, "TODO") || !strings.Contains(strings.ToLower(text), "temporary") {
		t.Fatal("proto must stay marked temporary until it moves to the shared repository")
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
