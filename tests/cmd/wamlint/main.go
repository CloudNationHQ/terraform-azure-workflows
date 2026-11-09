// Command wamlint runs the WAM convention checks on one or more module
// directories. It is used for the baseline across all module repos and to
// generate lint/CONVENTIONS.md; CI runs the same checks via TestModuleConventions.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cloudnationhq/terraform-azure-workflows/conventions"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("wamlint", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	format := flags.String("format", "text", "output format: text, json, or markdown")
	rules := flags.Bool("rules", false, "print the convention rules as Markdown")
	repoName := flags.String("repo", "", "repository name (requires exactly one DIR)")
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: wamlint [flags] DIR [DIR ...]")
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *rules {
		fmt.Print(conventions.RulesMarkdown())
		return 0
	}

	dirs := flags.Args()
	if len(dirs) == 0 {
		flags.Usage()
		return 2
	}
	if *repoName != "" && len(dirs) > 1 {
		fmt.Fprintln(os.Stderr, "wamlint: -repo requires exactly one DIR")
		return 2
	}
	switch *format {
	case "text", "json", "markdown":
	default:
		fmt.Fprintf(os.Stderr, "wamlint: invalid format %q (want text, json, or markdown)\n", *format)
		return 2
	}

	results := make([]conventions.RepoResult, 0, len(dirs))
	for _, dir := range dirs {
		repo := *repoName
		if repo == "" {
			repo = filepath.Base(filepath.Clean(dir))
		}
		findings, err := conventions.Check(dir, conventions.Options{RepoName: repo})
		if findings == nil {
			findings = []conventions.Finding{}
		}
		result := conventions.RepoResult{Repo: repo, Findings: findings}
		if err != nil {
			result.Error = err.Error()
		}
		results = append(results, result)
	}

	switch *format {
	case "json":
		data, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "wamlint: error: %s\n", err)
			return 1
		}
		fmt.Println(string(data))
		return 0
	case "markdown":
		fmt.Print(conventions.BaselineMarkdown(results))
		return 0
	default:
		exitCode := 0
		for _, result := range results {
			for _, finding := range result.Findings {
				file := finding.File
				if len(dirs) > 1 {
					file = result.Repo + "/" + file
				}
				fmt.Printf("%s:%d: %s %s %s\n",
					file, finding.Line, finding.Severity, finding.RuleID, finding.Message)
				if finding.Severity == conventions.SeverityError {
					exitCode = 1
				}
			}
			if result.Error != "" {
				fmt.Fprintf(os.Stderr, "%s: error: %s\n", result.Repo, result.Error)
				exitCode = 1
			}
		}
		return exitCode
	}
}
