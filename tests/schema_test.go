package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"golang.org/x/exp/slices"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Schema definitions
type TerraformSchema struct {
	ProviderSchemas map[string]*ProviderSchema `json:"provider_schemas"`
}

type ProviderSchema struct {
	ResourceSchemas   map[string]*ResourceSchema `json:"resource_schemas"`
	DataSourceSchemas map[string]*ResourceSchema `json:"data_source_schemas"`
}

type ResourceSchema struct {
	Block *SchemaBlock `json:"block"`
}

type SchemaBlock struct {
	Attributes map[string]*SchemaAttribute `json:"attributes"`
	BlockTypes map[string]*SchemaBlockType `json:"block_types"`
}

type SchemaAttribute struct {
	Required bool `json:"required"`
	Optional bool `json:"optional"`
	Computed bool `json:"computed"`
}

type SchemaBlockType struct {
	Nesting  string       `json:"nesting"`
	MinItems int          `json:"min_items"`
	MaxItems int          `json:"max_items"`
	Block    *SchemaBlock `json:"block"`
}

// Terraform resource structures
type ValidationFinding struct {
	ResourceType  string
	Path          string
	Name          string
	Required      bool
	IsBlock       bool
	IsDataSource  bool
	SubmoduleName string
}

type ProviderConfig struct {
	Source  string
	Version string
}

type SubModule struct {
	name string
	path string
}

type ParsedResource struct {
	Type string
	Name string
	data BlockData
}

type ParsedDataSource struct {
	Type string
	Name string
	data BlockData
}

type BlockData struct {
	properties    map[string]bool
	staticBlocks  map[string]*ParsedBlock
	dynamicBlocks map[string]*ParsedBlock
	ignoreChanges []string
}

type ParsedBlock struct {
	data BlockData
}

// Helper functions
func boolToStr(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}

func normalizeSource(source string) string {
	if strings.Contains(source, "/") && !strings.Contains(source, "registry.terraform.io/") {
		return "registry.terraform.io/" + source
	}
	return source
}

func isIgnored(ignore []string, name string) bool {
	if slices.Contains(ignore, "*all*") {
		return true
	}
	for _, item := range ignore {
		if strings.EqualFold(item, name) {
			return true
		}
	}
	return false
}

func findContentBlock(body *hclsyntax.Body) *hclsyntax.Body {
	for _, b := range body.Blocks {
		if b.Type == "content" {
			return b.Body
		}
	}
	return body
}

// Extract ignore_changes from cty.Value
func extractIgnoreChanges(val cty.Value) []string {
	var changes []string
	if val.Type().IsCollectionType() {
		for it := val.ElementIterator(); it.Next(); {
			_, v := it.Element()
			if v.Type() == cty.String {
				str := v.AsString()
				if str == "all" {
					return []string{"*all*"}
				}
				changes = append(changes, str)
			}
		}
	}
	return changes
}

// Extract ignore_changes directly from AST
func extractLifecycleIgnoreChangesFromAST(body *hclsyntax.Body) []string {
	var ignoreChanges []string
	for _, block := range body.Blocks {
		if block.Type == "lifecycle" {
			for name, attr := range block.Body.Attributes {
				if name == "ignore_changes" {
					if listExpr, ok := attr.Expr.(*hclsyntax.TupleConsExpr); ok {
						for _, expr := range listExpr.Exprs {
							switch exprType := expr.(type) {
							case *hclsyntax.ScopeTraversalExpr:
								if len(exprType.Traversal) > 0 {
									ignoreChanges = append(ignoreChanges, exprType.Traversal.RootName())
								}
							case *hclsyntax.TemplateExpr:
								if len(exprType.Parts) == 1 {
									if literalPart, ok := exprType.Parts[0].(*hclsyntax.LiteralValueExpr); ok && literalPart.Val.Type() == cty.String {
										ignoreChanges = append(ignoreChanges, literalPart.Val.AsString())
									}
								}
							case *hclsyntax.LiteralValueExpr:
								if exprType.Val.Type() == cty.String {
									ignoreChanges = append(ignoreChanges, exprType.Val.AsString())
								}
							}
						}
					}
				}
			}
		}
	}
	return ignoreChanges
}

// BlockData methods
func NewBlockData() BlockData {
	return BlockData{
		properties:    make(map[string]bool),
		staticBlocks:  make(map[string]*ParsedBlock),
		dynamicBlocks: make(map[string]*ParsedBlock),
		ignoreChanges: []string{},
	}
}

func (bd *BlockData) ParseAttributes(body *hclsyntax.Body) {
	for name := range body.Attributes {
		bd.properties[name] = true
	}
}

func (bd *BlockData) ParseBlocks(body *hclsyntax.Body) {
	directIgnoreChanges := extractLifecycleIgnoreChangesFromAST(body)
	if len(directIgnoreChanges) > 0 {
		bd.ignoreChanges = append(bd.ignoreChanges, directIgnoreChanges...)
	}

	for _, block := range body.Blocks {
		switch block.Type {
		case "lifecycle":
			for name, attr := range block.Body.Attributes {
				if name == "ignore_changes" {
					if val, diags := attr.Expr.Value(nil); diags == nil || !diags.HasErrors() {
						bd.ignoreChanges = append(bd.ignoreChanges, extractIgnoreChanges(val)...)
					}
				}
			}
		case "dynamic":
			if len(block.Labels) == 1 {
				contentBlock := findContentBlock(block.Body)
				parsed := ParseSyntaxBody(contentBlock)
				name := block.Labels[0]
				if existing := bd.dynamicBlocks[name]; existing != nil {
					mergeBlocks(existing, parsed)
				} else {
					bd.dynamicBlocks[name] = parsed
				}
			}
		default:
			parsed := ParseSyntaxBody(block.Body)
			bd.staticBlocks[block.Type] = parsed
		}
	}
}

func (bd *BlockData) Validate(
	t *testing.T,
	resourceType, path string,
	schema *SchemaBlock,
	parentIgnore []string,
	findings *[]ValidationFinding,
) {
	if schema == nil {
		return
	}

	ignore := slices.Clone(parentIgnore)
	ignore = append(ignore, bd.ignoreChanges...)

	// Validate attributes
	for name, attr := range schema.Attributes {
		if name == "id" || (attr.Computed && !attr.Optional && !attr.Required) || isIgnored(ignore, name) {
			continue
		}

		if !bd.properties[name] {
			*findings = append(*findings, ValidationFinding{
				ResourceType: resourceType,
				Path:         path,
				Name:         name,
				Required:     attr.Required,
				IsBlock:      false,
			})
		}
	}

	// Validate blocks
	for name, blockType := range schema.BlockTypes {
		if name == "timeouts" || isIgnored(ignore, name) {
			continue
		}

		static := bd.staticBlocks[name]
		dynamic := bd.dynamicBlocks[name]

		if static == nil && dynamic == nil {
			*findings = append(*findings, ValidationFinding{
				ResourceType: resourceType,
				Path:         path,
				Name:         name,
				Required:     blockType.MinItems > 0,
				IsBlock:      true,
			})
			continue
		}

		target := static
		if target == nil {
			target = dynamic
		}

		newPath := fmt.Sprintf("%s.%s", path, name)
		target.data.Validate(t, resourceType, newPath, blockType.Block, ignore, findings)
	}
}

func mergeBlocks(dest, src *ParsedBlock) {
	for k := range src.data.properties {
		dest.data.properties[k] = true
	}

	for k, v := range src.data.staticBlocks {
		if existing, ok := dest.data.staticBlocks[k]; ok {
			mergeBlocks(existing, v)
		} else {
			dest.data.staticBlocks[k] = v
		}
	}

	for k, v := range src.data.dynamicBlocks {
		if existing, ok := dest.data.dynamicBlocks[k]; ok {
			mergeBlocks(existing, v)
		} else {
			dest.data.dynamicBlocks[k] = v
		}
	}

	dest.data.ignoreChanges = append(dest.data.ignoreChanges, src.data.ignoreChanges...)
}

func ParseSyntaxBody(body *hclsyntax.Body) *ParsedBlock {
	bd := NewBlockData()
	blk := &ParsedBlock{data: bd}
	bd.ParseAttributes(body)
	bd.ParseBlocks(body)
	return blk
}

// HCL Parser
type DefaultHCLParser struct{}

func (p *DefaultHCLParser) ParseProviderRequirements(filename string) (map[string]ProviderConfig, error) {
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		return map[string]ProviderConfig{}, nil
	}

	parser := hclparse.NewParser()
	f, diags := parser.ParseHCLFile(filename)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse error in file %s: %v", filename, diags)
	}

	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("invalid body in file %s", filename)
	}

	providers := make(map[string]ProviderConfig)
	for _, blk := range body.Blocks {
		if blk.Type == "terraform" {
			for _, innerBlk := range blk.Body.Blocks {
				if innerBlk.Type == "required_providers" {
					attrs, _ := innerBlk.Body.JustAttributes()
					for name, attr := range attrs {
						val, _ := attr.Expr.Value(nil)
						if val.Type().IsObjectType() {
							pc := ProviderConfig{}
							if sourceVal := val.GetAttr("source"); !sourceVal.IsNull() {
								pc.Source = normalizeSource(sourceVal.AsString())
							}
							if versionVal := val.GetAttr("version"); !versionVal.IsNull() {
								pc.Version = versionVal.AsString()
							}
							providers[name] = pc
						}
					}
				}
			}
		}
	}
	return providers, nil
}

func (p *DefaultHCLParser) ParseMainFile(filename string) ([]ParsedResource, []ParsedDataSource, error) {
	parser := hclparse.NewParser()
	f, diags := parser.ParseHCLFile(filename)
	if diags.HasErrors() {
		return nil, nil, fmt.Errorf("parse error in file %s: %v", filename, diags)
	}

	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return nil, nil, fmt.Errorf("invalid body in file %s", filename)
	}

	var resources []ParsedResource
	var dataSources []ParsedDataSource

	for _, blk := range body.Blocks {
		if (blk.Type == "resource" || blk.Type == "data") && len(blk.Labels) >= 2 {
			parsed := ParseSyntaxBody(blk.Body)
			ignoreChanges := extractLifecycleIgnoreChangesFromAST(blk.Body)
			if len(ignoreChanges) > 0 {
				parsed.data.ignoreChanges = append(parsed.data.ignoreChanges, ignoreChanges...)
			}

			if blk.Type == "resource" {
				resources = append(resources, ParsedResource{
					Type: blk.Labels[0],
					Name: blk.Labels[1],
					data: parsed.data,
				})
			} else {
				dataSources = append(dataSources, ParsedDataSource{
					Type: blk.Labels[0],
					Name: blk.Labels[1],
					data: parsed.data,
				})
			}
		}
	}
	return resources, dataSources, nil
}

// Validation functions
func findSubmodules(modulesDir string) ([]SubModule, error) {
	var result []SubModule
	entries, err := os.ReadDir(modulesDir)
	if err != nil {
		return result, nil // Return empty slice for non-existent directory
	}

	for _, e := range entries {
		if e.IsDir() {
			subName := e.Name()
			subPath := filepath.Join(modulesDir, subName)
			mainTf := filepath.Join(subPath, "main.tf")
			if _, err := os.Stat(mainTf); err == nil {
				result = append(result, SubModule{subName, subPath})
			}
		}
	}
	return result, nil
}

func validateResources(t *testing.T, resources []ParsedResource, tfSchema TerraformSchema, providers map[string]ProviderConfig, dir, submoduleName string) []ValidationFinding {
	var findings []ValidationFinding

	for _, r := range resources {
		provName := strings.SplitN(r.Type, "_", 2)[0]
		cfg, ok := providers[provName]
		if !ok {
			t.Logf("No provider config for resource type %s in %s", r.Type, dir)
			continue
		}

		pSchema := tfSchema.ProviderSchemas[cfg.Source]
		if pSchema == nil {
			t.Logf("No provider schema found for source %s in %s", cfg.Source, dir)
			continue
		}

		resSchema := pSchema.ResourceSchemas[r.Type]
		if resSchema == nil {
			t.Logf("No resource schema found for %s in provider %s (dir=%s)", r.Type, cfg.Source, dir)
			continue
		}

		var local []ValidationFinding
		r.data.Validate(t, r.Type, "root", resSchema.Block, r.data.ignoreChanges, &local)

		for i := range local {
			shouldExclude := false
			for _, ignored := range r.data.ignoreChanges {
				if strings.EqualFold(ignored, local[i].Name) {
					shouldExclude = true
					break
				}
			}

			if !shouldExclude {
				local[i].SubmoduleName = submoduleName
				findings = append(findings, local[i])
			}
		}
	}

	return findings
}

func validateDataSources(t *testing.T, dataSources []ParsedDataSource, tfSchema TerraformSchema, providers map[string]ProviderConfig, dir, submoduleName string) []ValidationFinding {
	var findings []ValidationFinding

	for _, ds := range dataSources {
		provName := strings.SplitN(ds.Type, "_", 2)[0]
		cfg, ok := providers[provName]
		if !ok {
			t.Logf("No provider config for data source type %s in %s", ds.Type, dir)
			continue
		}

		pSchema := tfSchema.ProviderSchemas[cfg.Source]
		if pSchema == nil {
			t.Logf("No provider schema found for source %s in %s", cfg.Source, dir)
			continue
		}

		dsSchema := pSchema.DataSourceSchemas[ds.Type]
		if dsSchema == nil {
			t.Logf("No data source schema found for %s in provider %s (dir=%s)", ds.Type, cfg.Source, dir)
			continue
		}

		var local []ValidationFinding
		ds.data.Validate(t, ds.Type, "root", dsSchema.Block, ds.data.ignoreChanges, &local)

		for i := range local {
			shouldExclude := false
			for _, ignored := range ds.data.ignoreChanges {
				if strings.EqualFold(ignored, local[i].Name) {
					shouldExclude = true
					break
				}
			}

			if !shouldExclude {
				local[i].SubmoduleName = submoduleName
				local[i].IsDataSource = true
				findings = append(findings, local[i])
			}
		}
	}

	return findings
}

func validateTerraformSchemaInDir(t *testing.T, dir, submoduleName string) ([]ValidationFinding, error) {
	mainTf := filepath.Join(dir, "main.tf")
	if _, err := os.Stat(mainTf); os.IsNotExist(err) {
		return nil, nil
	}

	parser := &DefaultHCLParser{}
	tfFile := filepath.Join(dir, "terraform.tf")
	providers, err := parser.ParseProviderRequirements(tfFile)
	if err != nil {
		return nil, fmt.Errorf("failed to parse provider config in %s: %w", dir, err)
	}

	// Set up cleanup
	defer func() {
		os.RemoveAll(filepath.Join(dir, ".terraform"))
		os.Remove(filepath.Join(dir, "terraform.tfstate"))
		os.Remove(filepath.Join(dir, ".terraform.lock.hcl"))
	}()

	// Initialize terraform
	initCmd := exec.CommandContext(context.Background(), "terraform", "init")
	initCmd.Dir = dir
	if out, err := initCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("terraform init failed in %s: %v\nOutput: %s", dir, err, string(out))
	}

	// Get schema
	schemaCmd := exec.CommandContext(context.Background(), "terraform", "providers", "schema", "-json")
	schemaCmd.Dir = dir
	out, err := schemaCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get schema in %s: %w", dir, err)
	}

	var tfSchema TerraformSchema
	if err := json.Unmarshal(out, &tfSchema); err != nil {
		return nil, fmt.Errorf("failed to unmarshal schema in %s: %w", dir, err)
	}

	resources, dataSources, err := parser.ParseMainFile(mainTf)
	if err != nil {
		return nil, fmt.Errorf("parseMainFile in %s: %w", dir, err)
	}

	var findings []ValidationFinding
	findings = append(findings, validateResources(t, resources, tfSchema, providers, dir, submoduleName)...)
	findings = append(findings, validateDataSources(t, dataSources, tfSchema, providers, dir, submoduleName)...)

	return findings, nil
}

func deduplicateFindings(findings []ValidationFinding) []ValidationFinding {
	seen := make(map[string]bool)
	var result []ValidationFinding

	for _, f := range findings {
		key := fmt.Sprintf("%s|%s|%s|%v|%v|%s", f.ResourceType, f.Path, f.Name, f.IsBlock, f.IsDataSource, f.SubmoduleName)
		if !seen[key] {
			seen[key] = true
			result = append(result, f)
		}
	}

	return result
}

// GitHub issue management
type GitHubIssueService struct {
	RepoOwner string
	RepoName  string
	token     string
	Client    *http.Client
}

func (g *GitHubIssueService) CreateOrUpdateIssue(findings []ValidationFinding) error {
	if len(findings) == 0 {
		return nil
	}

	const header = "### \n\n"
	dedup := make(map[string]ValidationFinding)

	// Deduplicate exact lines
	for _, f := range findings {
		key := fmt.Sprintf("%s|%s|%s|%v|%v|%s",
			f.ResourceType,
			strings.ReplaceAll(f.Path, "root.", ""),
			f.Name,
			f.IsBlock,
			f.IsDataSource,
			f.SubmoduleName,
		)
		dedup[key] = f
	}

	var newBody bytes.Buffer
	fmt.Fprint(&newBody, header)
	for _, f := range dedup {
		cleanPath := strings.ReplaceAll(f.Path, "root.", "")
		status := boolToStr(f.Required, "required", "optional")
		itemType := boolToStr(f.IsBlock, "block", "property")
		entityType := boolToStr(f.IsDataSource, "data source", "resource")

		if f.SubmoduleName == "" {
			fmt.Fprintf(&newBody, "`%s`: missing %s %s `%s` in `%s` (%s)\n\n",
				f.ResourceType, status, itemType, f.Name, cleanPath, entityType)
		} else {
			fmt.Fprintf(&newBody, "`%s`: missing %s %s `%s` in `%s` in submodule `%s` (%s)\n\n",
				f.ResourceType, status, itemType, f.Name, cleanPath, f.SubmoduleName, entityType)
		}
	}

	title := "Generated schema validation"
	issueNum, existingBody, err := g.findExistingIssue(title)
	if err != nil {
		return err
	}

	finalBody := newBody.String()
	if issueNum > 0 {
		parts := strings.SplitN(existingBody, header, 2)
		if len(parts) > 0 {
			finalBody = strings.TrimSpace(parts[0]) + "\n\n" + newBody.String()
		}
		return g.updateIssue(issueNum, finalBody)
	}

	return g.createIssue(title, finalBody)
}

func (g *GitHubIssueService) findExistingIssue(title string) (int, string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues?state=open", g.RepoOwner, g.RepoName)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "token "+g.token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := g.Client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("GitHub API error: %s", resp.Status)
	}

	var issues []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&issues); err != nil {
		return 0, "", err
	}

	for _, issue := range issues {
		if issue.Title == title {
			return issue.Number, issue.Body, nil
		}
	}

	return 0, "", nil
}

func (g *GitHubIssueService) updateIssue(issueNumber int, body string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d", g.RepoOwner, g.RepoName, issueNumber)
	payload := struct {
		Body string `json:"body"`
	}{Body: body}

	data, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PATCH", url, bytes.NewReader(data))
	req.Header.Set("Authorization", "token "+g.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

func (g *GitHubIssueService) createIssue(title, body string) error {
	payload := struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}{
		Title: title,
		Body:  body,
	}

	data, _ := json.Marshal(payload)
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues", g.RepoOwner, g.RepoName)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(data))
	req.Header.Set("Authorization", "token "+g.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

// Repository info
type GitRepoInfo struct {
	terraformRoot string
}

func (g *GitRepoInfo) GetRepoInfo() (owner, repo string) {
	owner = os.Getenv("GITHUB_REPOSITORY_OWNER")
	repo = os.Getenv("GITHUB_REPOSITORY_NAME")
	if owner != "" && repo != "" {
		return owner, repo
	}

	if ghRepo := os.Getenv("GITHUB_REPOSITORY"); ghRepo != "" {
		parts := strings.SplitN(ghRepo, "/", 2)
		if len(parts) == 2 {
			return parts[0], parts[1]
		}
	}

	return "", ""
}

// Main test function
func TestValidateTerraformSchema(t *testing.T) {
	// Get root directory
	terraformRoot := os.Getenv("TERRAFORM_ROOT")
	if terraformRoot == "" {
		terraformRoot = "."
	}

	// Validate root
	rootFindings, err := validateTerraformSchemaInDir(t, terraformRoot, "")
	if err != nil {
		t.Fatalf("Failed to validate root at %s: %v", terraformRoot, err)
	}

	var allFindings []ValidationFinding
	allFindings = append(allFindings, rootFindings...)

	// Validate submodules
	modulesDir := filepath.Join(terraformRoot, "modules")
	subs, err := findSubmodules(modulesDir)
	if err != nil {
		t.Fatalf("Failed to find submodules in %s: %v", modulesDir, err)
	}

	for _, sm := range subs {
		f, sErr := validateTerraformSchemaInDir(t, sm.path, sm.name)
		if sErr != nil {
			t.Errorf("Failed to validate submodule %s: %v", sm.name, sErr)
			continue
		}
		allFindings = append(allFindings, f...)
	}

	// Deduplicate findings
	deduplicatedFindings := deduplicateFindings(allFindings)

	// Log all missing items
	for _, f := range deduplicatedFindings {
		place := "root"
		if f.SubmoduleName != "" {
			place = "root in submodule " + f.SubmoduleName
		}
		requiredOptional := boolToStr(f.Required, "required", "optional")
		blockOrProp := boolToStr(f.IsBlock, "block", "property")
		entityType := boolToStr(f.IsDataSource, "data source", "resource")

		t.Logf("%s missing %s %s %q in %s (%s)", f.ResourceType, requiredOptional, blockOrProp, f.Name, place, entityType)
	}

	// Create/update GitHub issue if token is set
	if ghToken := os.Getenv("GITHUB_TOKEN"); ghToken != "" && len(deduplicatedFindings) > 0 {
		gi := &GitRepoInfo{terraformRoot: terraformRoot}
		owner, repoName := gi.GetRepoInfo()

		if owner != "" && repoName != "" {
			gh := &GitHubIssueService{
				RepoOwner: owner,
				RepoName:  repoName,
				token:     ghToken,
				Client:    &http.Client{Timeout: 10 * time.Second},
			}

			if err := gh.CreateOrUpdateIssue(deduplicatedFindings); err != nil {
				t.Errorf("Failed to create/update GitHub issue: %v", err)
			}
		} else {
			t.Log("Could not determine repository info for GitHub issue creation.")
		}
	}

	// Fail if any missing items found
	if len(deduplicatedFindings) > 0 {
		t.Fatalf("Found %d missing properties/blocks in root or submodules. See logs above.", len(deduplicatedFindings))
	}
}
