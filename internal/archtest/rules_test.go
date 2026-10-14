package archtest

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const rulesHeading = "## 규칙표"

// ruleTable reads the rule | enforcer table under rulesHeading. It returns
// the row count, the rows whose enforcer starts with "Judgment", and every
// row that has no enforcer.
func ruleTable(doc string) (rows, judgment int, bad []string, err error) {
	on, seen := false, 0
	for _, line := range strings.Split(doc, "\n") {
		switch {
		case line == rulesHeading:
			on = true
			continue
		case on && strings.HasPrefix(line, "## "):
			on = false
		}
		if !on || !strings.HasPrefix(line, "|") {
			continue
		}
		if seen++; seen <= 2 { // header and separator
			continue
		}
		rows++
		cells := strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "|"), "|"), "|")
		if len(cells) != 2 || strings.TrimSpace(cells[1]) == "" {
			bad = append(bad, fmt.Sprintf("row %d has no enforcer: %s", rows, line))
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(cells[1]), "Judgment") {
			judgment++
		}
	}
	if seen == 0 {
		return 0, 0, nil, fmt.Errorf("no table under %q", rulesHeading)
	}
	return rows, judgment, bad, nil
}

// TestEveryRuleHasAnEnforcer: every AGENTS.md rule names a test, lint, or CI
// enforcer, or says "Judgment". The Judgment count is logged.
func TestEveryRuleHasAnEnforcer(t *testing.T) {
	probe := "# t\n" + rulesHeading + "\n\n| rule | enforcer |\n| --- | --- |\n| a | `TestA` |\n| b | Judgment. why |\n| c |  |\n| d |\n\n## next\n| x | |\n"
	rows, judgment, bad, err := ruleTable(probe)
	want := []string{"row 3 has no enforcer: | c |  |", "row 4 has no enforcer: | d |"}
	if err != nil || rows != 4 || judgment != 1 || !reflect.DeepEqual(bad, want) {
		t.Fatalf("probe: rows %d judgment %d bad %q err %v", rows, judgment, bad, err)
	}
	if _, _, _, err := ruleTable("# t\n## other\n| a | b |\n"); err == nil {
		t.Fatal("probe: a missing rule table was accepted")
	}

	doc, err := os.ReadFile(filepath.Join(repoRoot(t), "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows, judgment, bad, err = ruleTable(string(doc))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bad {
		t.Error(b)
	}
	t.Logf("rules: %d rows, %d Judgment", rows, judgment)
}
