package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cloudnationhq/terraform-azure-workflows/conventions"
)

// TestModuleConventions runs the WAM convention checks on MODULE_PATH.
// WAM_CONVENTIONS_MODE=warn (default) reports findings as warnings only;
// enforce fails on error-severity findings.
func TestModuleConventions(t *testing.T) {
	dir := os.Getenv("MODULE_PATH")
	if dir == "" {
		t.Skip("MODULE_PATH not set")
	}

	mode := os.Getenv("WAM_CONVENTIONS_MODE")
	if mode == "" {
		mode = "warn"
	}
	if mode != "warn" && mode != "enforce" {
		t.Fatalf("invalid WAM_CONVENTIONS_MODE %q: want warn or enforce", mode)
	}

	repo := ""
	if _, name, ok := strings.Cut(os.Getenv("GITHUB_REPOSITORY"), "/"); ok {
		repo = name
	}

	findings, err := conventions.Check(dir, conventions.Options{RepoName: repo})
	if err != nil {
		t.Fatalf("checking module conventions: %v", err)
	}
	t.Logf("mode=%s findings=%d", mode, len(findings))

	errorCount := 0
	for _, f := range findings {
		rule, _ := conventions.RuleByID(f.RuleID)
		text := fmt.Sprintf("%s (WAM rule %s)", f.Message, rule.Standard)
		if f.Severity == conventions.SeverityError {
			errorCount++
		}
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			level := "warning"
			if mode == "enforce" && f.Severity == conventions.SeverityError {
				level = "error"
			}
			fmt.Printf("::%s file=%s,line=%d,title=%s::%s\n",
				level, escapeProperty(f.File), f.Line, f.RuleID, escapeData(text))
		} else {
			t.Logf("%s:%d: %s %s %s", f.File, f.Line, f.Severity, f.RuleID, text)
		}
	}

	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		writeSummary(t, path, mode, findings)
	}

	if mode == "enforce" && errorCount > 0 {
		t.Errorf("%d convention error(s) in enforce mode", errorCount)
	}
}

func writeSummary(t *testing.T, path, mode string, findings []conventions.Finding) {
	t.Helper()
	cell := func(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

	var summary strings.Builder
	fmt.Fprintf(&summary, "## WAM conventions (%s mode)\n\n", mode)
	if len(findings) == 0 {
		summary.WriteString("No convention findings.\n")
	} else {
		summary.WriteString("| Rule | Severity | File | Line | Finding |\n")
		summary.WriteString("| --- | --- | --- | --- | --- |\n")
		for _, f := range findings {
			fmt.Fprintf(&summary, "| [%s](%s) | %s | %s | %d | %s |\n",
				f.RuleID, conventions.StandardURL, f.Severity, cell(f.File), f.Line, cell(f.Message))
		}
	}
	fmt.Fprintf(&summary, "\nSee [the checks](https://github.com/CloudNationHQ/terraform-azure-workflows/blob/main/lint/CONVENTIONS.md) and [the WAM standard](%s).\n",
		conventions.StandardURL)

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("opening step summary: %v", err)
	}
	defer file.Close()
	if _, err := file.WriteString(summary.String()); err != nil {
		t.Fatalf("writing step summary: %v", err)
	}
}

func escapeData(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}

func escapeProperty(s string) string {
	return strings.NewReplacer(":", "%3A", ",", "%2C").Replace(escapeData(s))
}
