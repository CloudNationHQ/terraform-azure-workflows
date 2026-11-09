package conventions

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

var (
	movedFilename = regexp.MustCompile(`^moved_v\d+_to_v\d+\.tf$`)
	ignorePattern = regexp.MustCompile(`^(#|//)\s*wam-lint-ignore\s+([A-Za-z0-9_,]+)\s*(.*)$`)
	ignorePrefix  = regexp.MustCompile(`^(#|//)\s*wam-lint-ignore\b`)
)

type comment struct {
	startLine int
	endLine   int
	text      string
}

type ignore struct {
	ids      map[string]bool
	line     int
	nextLine int
}

type fileCheck struct {
	file     string
	base     string
	root     bool
	body     *hclsyntax.Body
	comments []comment
	lines    []string
	findings []Finding
}

// Check checks the root module and everything under modules/. Files that do
// not parse are skipped: terraform validate reports syntax errors.
func Check(moduleDir string, opts Options) ([]Finding, error) {
	var findings []Finding

	err := filepath.WalkDir(moduleDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(moduleDir, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if rel == "." {
				return nil
			}
			switch entry.Name() {
			case ".terraform", ".git", "examples", "tests":
				return filepath.SkipDir
			}
			if strings.Split(filepath.ToSlash(rel), "/")[0] != "modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(entry.Name()) != ".tf" {
			return nil
		}

		src, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		parsed, diags := hclsyntax.ParseConfig(src, path, hcl.InitialPos)
		if diags.HasErrors() {
			return nil
		}
		body, ok := parsed.Body.(*hclsyntax.Body)
		if !ok {
			return nil
		}

		f := &fileCheck{
			file:     filepath.ToSlash(rel),
			base:     entry.Name(),
			root:     filepath.Dir(rel) == ".",
			body:     body,
			comments: lexComments(src, path),
			lines:    strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n"),
		}
		f.checkWAM001()
		f.checkWAM002()
		f.checkWAM003()
		f.checkWAM004()
		f.checkWAM005()
		f.checkWAM006(opts)
		f.checkWAM007()
		f.checkWAM008()
		f.checkWAM009()
		f.checkWAM010()
		f.checkWAM011()
		f.checkWAM012()
		f.checkWAM013()
		f.checkWAM014()

		findings = append(findings, f.applyIgnores(f.collectIgnores())...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.RuleID < b.RuleID
	})
	return findings, nil
}

func (f *fileCheck) add(id string, line int, message string) {
	rule, _ := RuleByID(id)
	f.findings = append(f.findings, Finding{
		RuleID: id, Severity: rule.Severity,
		File: f.file, Line: line, Message: message,
	})
}

func walkBlocks(body *hclsyntax.Body, visit func(*hclsyntax.Block)) {
	for _, block := range body.Blocks {
		visit(block)
		walkBlocks(block.Body, visit)
	}
}

func walkAttributes(body *hclsyntax.Body, visit func(*hclsyntax.Attribute)) {
	for _, attr := range body.Attributes {
		visit(attr)
	}
	for _, block := range body.Blocks {
		walkAttributes(block.Body, visit)
	}
}

func visitNodes(node hclsyntax.Node, visit func(hclsyntax.Node)) {
	hclsyntax.VisitAll(node, func(n hclsyntax.Node) hcl.Diagnostics {
		visit(n)
		return nil
	})
}

func (f *fileCheck) variableTypes(visit func(hclsyntax.Expression)) {
	walkBlocks(f.body, func(block *hclsyntax.Block) {
		if block.Type == "variable" {
			if attr := block.Body.Attributes["type"]; attr != nil {
				visit(attr.Expr)
			}
		}
	})
}

func isManagedBlock(block *hclsyntax.Block) bool {
	return block.Type == "resource" || block.Type == "data" || block.Type == "module"
}

func traversalIs(expr hclsyntax.Expression, name string) bool {
	traversal, ok := expr.(*hclsyntax.ScopeTraversalExpr)
	return ok && traversal.Traversal.RootName() == name
}

func keywordKey(expr hclsyntax.Expression) string {
	key, ok := expr.(*hclsyntax.ObjectConsKeyExpr)
	if !ok {
		return ""
	}
	return hcl.ExprAsKeyword(key.Wrapped)
}

func stringValue(expr hclsyntax.Expression) (string, bool) {
	value, diags := expr.Value(nil)
	if diags.HasErrors() || !value.IsKnown() || value.IsNull() || value.Type() != cty.String {
		return "", false
	}
	return value.AsString(), true
}

func (f *fileCheck) checkWAM001() {
	f.variableTypes(func(expr hclsyntax.Expression) {
		visitNodes(expr, func(node hclsyntax.Node) {
			if traversal, ok := node.(*hclsyntax.ScopeTraversalExpr); ok &&
				len(traversal.Traversal) == 1 && traversal.Traversal.RootName() == "any" {
				f.add("WAM001", traversal.Range().Start.Line, "replace any with an explicit type")
			}
		})
	})
}

func (f *fileCheck) checkWAM002() {
	walkBlocks(f.body, func(block *hclsyntax.Block) {
		if isManagedBlock(block) {
			if attr := block.Body.Attributes["count"]; attr != nil {
				f.add("WAM002", attr.SrcRange.Start.Line, "use for_each instead of count")
			}
		}
	})
}

func (f *fileCheck) checkWAM003() {
	f.variableTypes(func(expr hclsyntax.Expression) {
		visitNodes(expr, func(node hclsyntax.Node) {
			call, ok := node.(*hclsyntax.FunctionCallExpr)
			if !ok || call.Name != "optional" || len(call.Args) != 2 ||
				!traversalIs(call.Args[0], "bool") {
				return
			}
			value, ok := call.Args[1].(*hclsyntax.LiteralValueExpr)
			if ok && value.Val.RawEquals(cty.True) {
				f.add("WAM003", call.Range().Start.Line,
					"remove the true default; let the caller set it explicitly")
			}
		})
	})
}

func constantDefault(expr hclsyntax.Expression) bool {
	switch value := expr.(type) {
	case *hclsyntax.LiteralValueExpr:
		return !value.Val.IsNull()
	case *hclsyntax.TemplateExpr:
		return value.IsStringLiteral()
	case *hclsyntax.ObjectConsExpr:
		return len(value.Items) == 0
	case *hclsyntax.TupleConsExpr:
		return len(value.Exprs) == 0
	default:
		return false
	}
}

func (f *fileCheck) checkWAM004() {
	if f.base != "main.tf" {
		return
	}
	visitNodes(f.body, func(node hclsyntax.Node) {
		call, ok := node.(*hclsyntax.FunctionCallExpr)
		if ok && call.Name == "try" && len(call.Args) >= 2 &&
			constantDefault(call.Args[len(call.Args)-1]) {
			f.add("WAM004", call.Range().Start.Line,
				"move the default to variables.tf (optional(type, default)) or use lookup()")
		}
	})
}

func (f *fileCheck) checkConstraint(expr hclsyntax.Expression) {
	value, ok := stringValue(expr)
	if !ok {
		return
	}
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" && !strings.HasPrefix(part, "~>") {
			f.add("WAM005", expr.Range().Start.Line,
				fmt.Sprintf("use a ~> constraint instead of %q", value))
			return
		}
	}
}

func (f *fileCheck) checkWAM005() {
	if f.base != "terraform.tf" {
		return
	}
	for _, block := range f.body.Blocks {
		if block.Type != "terraform" {
			continue
		}
		if attr := block.Body.Attributes["required_version"]; attr != nil {
			f.checkConstraint(attr.Expr)
		}
		for _, nested := range block.Body.Blocks {
			if nested.Type != "required_providers" {
				continue
			}
			for _, attr := range nested.Body.Attributes {
				object, ok := attr.Expr.(*hclsyntax.ObjectConsExpr)
				if !ok {
					continue
				}
				for _, item := range object.Items {
					if objectKey(item.KeyExpr) == "version" {
						f.checkConstraint(item.ValueExpr)
					}
				}
			}
		}
	}
}

func (f *fileCheck) checkWAM006(opts Options) {
	if opts.RepoName == "terraform-azure-uai" {
		return
	}
	walkBlocks(f.body, func(block *hclsyntax.Block) {
		if block.Type == "resource" && len(block.Labels) > 0 &&
			block.Labels[0] == "azurerm_user_assigned_identity" {
			f.add("WAM006", block.TypeRange.Start.Line,
				"pass identity IDs in; create UAIs with terraform-azure-uai")
		}
	})
}

func (f *fileCheck) checkWAM007() {
	walkBlocks(f.body, func(block *hclsyntax.Block) {
		if (block.Type == "resource" || block.Type == "data") &&
			len(block.Labels) > 0 && strings.HasPrefix(block.Labels[0], "azapi_") &&
			!f.explained(block.Range()) {
			f.add("WAM007", block.TypeRange.Start.Line,
				"explain in a comment why azapi is needed instead of azurerm")
		}
	})
}

func (f *fileCheck) checkWAM008() {
	walkBlocks(f.body, func(block *hclsyntax.Block) {
		if block.Type != "resource" {
			return
		}
		walkBlocks(block.Body, func(nested *hclsyntax.Block) {
			if nested.Type == "lifecycle" && !f.explained(nested.Range()) {
				f.add("WAM008", nested.TypeRange.Start.Line,
					"explain in a comment why this lifecycle setting is needed")
			}
		})
	})
}

func (f *fileCheck) checkWAM009() {
	walkBlocks(f.body, func(block *hclsyntax.Block) {
		if !isManagedBlock(block) {
			return
		}
		if attr := block.Body.Attributes["depends_on"]; attr != nil && !f.explained(attr.SrcRange) {
			f.add("WAM009", attr.SrcRange.Start.Line,
				"explain in a comment why depends_on is needed; Terraform usually infers order")
		}
	})
}

func (f *fileCheck) checkWAM010() {
	if movedFilename.MatchString(f.base) {
		return
	}
	walkBlocks(f.body, func(block *hclsyntax.Block) {
		if block.Type == "moved" {
			f.add("WAM010", block.TypeRange.Start.Line, "move this block to moved_vX_to_vY.tf")
		}
	})
}

func objectKey(expr hclsyntax.Expression) string {
	if keyword := keywordKey(expr); keyword != "" {
		return keyword
	}
	key, ok := expr.(*hclsyntax.ObjectConsKeyExpr)
	if !ok {
		return ""
	}
	template, ok := key.Wrapped.(*hclsyntax.TemplateExpr)
	if !ok || !template.IsStringLiteral() {
		return ""
	}
	value, _ := stringValue(template)
	return value
}

func (f *fileCheck) checkWAM011() {
	walkAttributes(f.body, func(attr *hclsyntax.Attribute) {
		if attr.Name != "for_each" {
			return
		}
		visitNodes(attr.Expr, func(node hclsyntax.Node) {
			if object, ok := node.(*hclsyntax.ObjectConsExpr); ok {
				for _, item := range object.Items {
					if objectKey(item.KeyExpr) == "default" {
						f.add("WAM011", item.KeyExpr.Range().Start.Line,
							`use "this" as the sentinel key`)
					}
				}
			}
		})
	})
}

func (f *fileCheck) checkWAM012() {
	if !f.root {
		return
	}
	for _, block := range f.body.Blocks {
		if block.Type != "variable" || len(block.Labels) == 0 {
			continue
		}
		switch block.Labels[0] {
		case "config", "instance", "environment", "naming":
			f.add("WAM012", block.TypeRange.Start.Line,
				"rename to a service noun; naming is handled by the caller")
		}
	}
}

func (f *fileCheck) checkWAM013() {
	if !f.root {
		return
	}
	for _, block := range f.body.Blocks {
		if block.Type != "variable" || len(block.Labels) == 0 {
			continue
		}
		switch block.Labels[0] {
		case "location", "resource_group_name", "tags":
			continue
		}
		if attr := block.Body.Attributes["default"]; attr != nil {
			f.add("WAM013", attr.SrcRange.Start.Line,
				"only location, resource_group_name and tags may have a default")
		}
	}
}

func (f *fileCheck) checkWAM014() {
	f.variableTypes(func(expr hclsyntax.Expression) {
		visitNodes(expr, func(node hclsyntax.Node) {
			object, ok := node.(*hclsyntax.ObjectConsExpr)
			if !ok {
				return
			}
			for _, item := range object.Items {
				name := keywordKey(item.KeyExpr)
				if name != "purge_protection_enabled" && name != "public_network_access_enabled" {
					continue
				}
				call, ok := item.ValueExpr.(*hclsyntax.FunctionCallExpr)
				if ok && call.Name == "optional" && len(call.Args) == 2 {
					f.add("WAM014", item.KeyExpr.Range().Start.Line,
						"security flag "+name+" must not have a default")
				}
			}
		})
	})
}

func lexComments(src []byte, filename string) []comment {
	tokens, _ := hclsyntax.LexConfig(src, filename, hcl.InitialPos)
	var comments []comment
	for _, token := range tokens {
		if token.Type != hclsyntax.TokenComment {
			continue
		}
		comments = append(comments, comment{
			startLine: token.Range.Start.Line,
			endLine:   token.Range.End.Line,
			text:      string(token.Bytes),
		})
	}
	return comments
}

// commentLines uses only the start line for # and // comments (their token
// includes the trailing newline). Block comments contribute every line they cover.
func commentLines(c comment) map[int]string {
	lines := map[int]string{}
	if !strings.HasPrefix(c.text, "/*") {
		lines[c.startLine] = strings.TrimSpace(c.text)
		return lines
	}
	parts := strings.Split(strings.TrimRight(c.text, "\r\n"), "\n")
	for offset, text := range parts {
		line := c.startLine + offset
		if line <= c.endLine {
			lines[line] = strings.TrimSpace(text)
		}
	}
	return lines
}

func stripComment(text string) string {
	text = strings.TrimSpace(text)
	for _, delimiter := range []string{"//", "/*", "#"} {
		text = strings.TrimPrefix(text, delimiter)
	}
	text = strings.TrimSuffix(strings.TrimSpace(text), "*/")
	return strings.TrimSpace(text)
}

// commentOnly reports whether a source line holds nothing but a comment, so a
// code line with a trailing comment does not count as part of a comment run.
func (f *fileCheck) commentOnly(line int) bool {
	if line < 1 || line > len(f.lines) {
		return false
	}
	text := strings.TrimSpace(f.lines[line-1])
	for _, prefix := range []string{"#", "//", "/*", "*"} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// explained reports whether a construct spanning rng has an explaining comment:
// a comment on or inside the construct, or a comment run directly above it that
// is more than a bare section header (one line of at most three words).
func (f *fileCheck) explained(rng hcl.Range) bool {
	lines := map[int][]string{}
	for _, c := range f.comments {
		if strings.Contains(c.text, "wam-lint-ignore") {
			continue
		}
		for line, text := range commentLines(c) {
			if line >= rng.Start.Line && line <= rng.End.Line {
				return true
			}
			lines[line] = append(lines[line], text)
		}
	}

	var run []string
	runLines := 0
	for line := rng.Start.Line - 1; line > 0; line-- {
		texts, ok := lines[line]
		if !ok || !f.commentOnly(line) {
			break
		}
		runLines++
		for _, text := range texts {
			run = append(run, stripComment(text))
		}
	}
	if runLines == 0 {
		return false
	}
	return runLines != 1 || len(strings.Fields(strings.Join(run, " "))) > 3
}

func (f *fileCheck) collectIgnores() []ignore {
	var ignores []ignore
	for _, c := range f.comments {
		text := strings.TrimRight(c.text, "\r\n")
		if !ignorePrefix.MatchString(text) {
			continue
		}
		match := ignorePattern.FindStringSubmatch(text)
		ids := map[string]bool{}
		valid := len(match) == 4
		if valid {
			valid = strings.TrimSpace(match[3]) != ""
			for _, id := range strings.Split(match[2], ",") {
				_, known := RuleByID(id)
				if !known || id == "WAM000" {
					valid = false
				}
				ids[id] = true
			}
		}
		if !valid {
			f.add("WAM000", c.startLine, "wam-lint-ignore needs known rule IDs and a reason")
			continue
		}

		nextLine := 0
		for line := c.startLine + 1; line <= len(f.lines); line++ {
			if strings.TrimSpace(f.lines[line-1]) == "" || f.commentOnly(line) {
				continue
			}
			nextLine = line
			break
		}
		ignores = append(ignores, ignore{ids: ids, line: c.startLine, nextLine: nextLine})
	}
	return ignores
}

func (f *fileCheck) applyIgnores(ignores []ignore) []Finding {
	var findings []Finding
	for _, finding := range f.findings {
		suppressed := false
		if finding.RuleID != "WAM000" {
			for _, directive := range ignores {
				if directive.ids[finding.RuleID] &&
					(finding.Line == directive.line || finding.Line == directive.nextLine) {
					suppressed = true
					break
				}
			}
		}
		if !suppressed {
			findings = append(findings, finding)
		}
	}
	return findings
}
