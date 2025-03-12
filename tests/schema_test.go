package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"sync"
	"testing"
	"time"
)

type BlockProcessor interface {
	ParseAttributes(body *hclsyntax.Body)
	ParseBlocks(body *hclsyntax.Body)
	Validate(t *testing.T, resourceType, path string, schema *SchemaBlock, parentIgnore []string, findings *[]ValidationFinding)
}

type HCLParser interface {
	ParseProviderRequirements(filename string) (map[string]ProviderConfig, error)
	ParseMainFile(filename string) ([]ParsedResource, []ParsedDataSource, error)
}

type IssueManager interface {
	CreateOrUpdateIssue(findings []ValidationFinding) error
}

type RepositoryInfoProvider interface {
	GetRepoInfo() (owner, name string)
}

// schema definitions
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

// ValidationFinding logs any missing attribute/block.
type ValidationFinding struct {
	ResourceType  string
	Path          string // e.g., "root" or "root.some_nested_block"
	Name          string
	Required      bool
	IsBlock       bool
	IsDataSource  bool   // If true, this is a data source, not a resource
	SubmoduleName string // empty => root, else submodule name
}

type ProviderConfig struct {
	Source  string
	Version string
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

// Added mutex for concurrent access protection
type GlobalMessagesStore struct {
	sync.Mutex
	messages map[string]bool
}

func NewGlobalMessagesStore() *GlobalMessagesStore {
	return &GlobalMessagesStore{
		messages: make(map[string]bool),
	}
}

func (s *GlobalMessagesStore) Add(key string) {
	s.Lock()
	defer s.Unlock()
	s.messages[key] = true
}

func (s *GlobalMessagesStore) GetAllMessages() []string {
	s.Lock()
	defer s.Unlock()

	var result []string
	for msg := range s.messages {
		result = append(result, msg)
	}
	return result
}

func NewBlockData() BlockData {
	return BlockData{
		properties:    make(map[string]bool),
		staticBlocks:  make(map[string]*ParsedBlock),
		dynamicBlocks: make(map[string]*ParsedBlock),
		ignoreChanges: []string{},
	}
}

// Extract ignore_changes directly from AST
func extractLifecycleIgnoreChangesFromAST(body *hclsyntax.Body) []string {
	if body == nil {
		return nil
	}

	var ignoreChanges []string

	for _, block := range body.Blocks {
		if block == nil || block.Body == nil {
			continue
		}

		if block.Type == "lifecycle" {
			for name, attr := range block.Body.Attributes {
				if name == "ignore_changes" && attr != nil && attr.Expr != nil {
					if listExpr, ok := attr.Expr.(*hclsyntax.TupleConsExpr); ok {
						for _, expr := range listExpr.Exprs {
							if expr == nil {
								continue
							}

							if traversalExpr, ok := expr.(*hclsyntax.ScopeTraversalExpr); ok {
								if len(traversalExpr.Traversal) > 0 {
									ignoreChanges = append(ignoreChanges, traversalExpr.Traversal.RootName())
								}
							} else if templateExpr, ok := expr.(*hclsyntax.TemplateExpr); ok {
								if len(templateExpr.Parts) == 1 && templateExpr.Parts[0] != nil {
									if literalPart, ok := templateExpr.Parts[0].(*hclsyntax.LiteralValueExpr); ok {
										if literalPart.Val.Type() == cty.String {
											ignoreChanges = append(ignoreChanges, literalPart.Val.AsString())
										}
									}
								}
							} else if literalExpr, ok := expr.(*hclsyntax.LiteralValueExpr); ok {
								if literalExpr.Val.Type() == cty.String {
									ignoreChanges = append(ignoreChanges, literalExpr.Val.AsString())
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

func (bd *BlockData) ParseAttributes(body *hclsyntax.Body) {
	if body == nil {
		return
	}

	for name := range body.Attributes {
		bd.properties[name] = true
	}
}

func (bd *BlockData) ParseBlocks(body *hclsyntax.Body) {
	if body == nil {
		return
	}

	// First, directly extract any lifecycle ignore_changes from AST
	directIgnoreChanges := extractLifecycleIgnoreChangesFromAST(body)
	if len(directIgnoreChanges) > 0 {
		bd.ignoreChanges = append(bd.ignoreChanges, directIgnoreChanges...)
	}

	for _, block := range body.Blocks {
		if block == nil {
			continue
		}

		switch block.Type {
		case "lifecycle":
			bd.parseLifecycle(block.Body)
		case "dynamic":
			if len(block.Labels) == 1 {
				bd.parseDynamicBlock(block.Body, block.Labels[0])
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
	if schema == nil || findings == nil {
		return
	}
	// Combine parent ignore changes with this block's ignore changes
	ignore := make([]string, 0, len(parentIgnore)+len(bd.ignoreChanges))
	ignore = append(ignore, parentIgnore...)
	ignore = append(ignore, bd.ignoreChanges...)

	bd.validateAttributes(t, resourceType, path, schema, ignore, findings)
	bd.validateBlocks(t, resourceType, path, schema, ignore, findings)
}

// parseLifecycle picks up ignore_changes in lifecycle { }
func (bd *BlockData) parseLifecycle(body *hclsyntax.Body) {
	if body == nil {
		return
	}

	for name, attr := range body.Attributes {
		if name == "ignore_changes" && attr != nil && attr.Expr != nil {
			val, diags := attr.Expr.Value(nil)
			// Only process if we could get a value without errors
			if diags == nil || !diags.HasErrors() {
				extracted := extractIgnoreChanges(val)
				bd.ignoreChanges = append(bd.ignoreChanges, extracted...)
			}
		}
	}
}

func (bd *BlockData) parseDynamicBlock(body *hclsyntax.Body, name string) {
	if body == nil || name == "" {
		return
	}

	contentBlock := findContentBlock(body)
	parsed := ParseSyntaxBody(contentBlock)
	if existing := bd.dynamicBlocks[name]; existing != nil {
		mergeBlocks(existing, parsed)
	} else {
		bd.dynamicBlocks[name] = parsed
	}
}

func (bd *BlockData) validateAttributes(
	t *testing.T,
	resType, path string,
	schema *SchemaBlock,
	ignore []string,
	findings *[]ValidationFinding,
) {
	if schema == nil || schema.Attributes == nil || findings == nil {
		return
	}

	for name, attr := range schema.Attributes {
		if attr == nil {
			continue
		}

		// Skip 'id' property as it's not useful to show
		if name == "id" {
			continue
		}

		// Skip purely computed attributes (those that are computed but not optional)
		// These are always exported, never set by the user
		if attr.Computed && !attr.Optional && !attr.Required {
			continue
		}

		// Skip attributes in the ignore list (case insensitive)
		if isIgnored(ignore, name) {
			continue
		}

		if !bd.properties[name] {
			*findings = append(*findings, ValidationFinding{
				ResourceType: resType,
				Path:         path,
				Name:         name,
				Required:     attr.Required,
				IsBlock:      false,
			})
			// No direct logging here to avoid duplication
		}
	}
}

func (bd *BlockData) validateBlocks(
	t *testing.T,
	resType, path string,
	schema *SchemaBlock,
	ignore []string,
	findings *[]ValidationFinding,
) {
	if schema == nil || schema.BlockTypes == nil || findings == nil {
		return
	}

	for name, blockType := range schema.BlockTypes {
		if blockType == nil {
			continue
		}

		// skip timeouts or ignored blocks (case insensitive)
		if name == "timeouts" || isIgnored(ignore, name) {
			continue
		}
		static := bd.staticBlocks[name]
		dynamic := bd.dynamicBlocks[name]
		if static == nil && dynamic == nil {
			*findings = append(*findings, ValidationFinding{
				ResourceType: resType,
				Path:         path,
				Name:         name,
				Required:     blockType.MinItems > 0,
				IsBlock:      true,
			})
			// No direct logging here to avoid duplication
			continue
		}
		var target *ParsedBlock
		if static != nil {
			target = static
		} else {
			target = dynamic
		}
		newPath := fmt.Sprintf("%s.%s", path, name)
		target.data.Validate(t, resType, newPath, blockType.Block, ignore, findings)
	}
}

// Helper function to check if an item is ignored (case insensitive)
func isIgnored(ignore []string, name string) bool {
	if name == "" {
		return false
	}

	// Check for the special "all" marker
	if slices.Contains(ignore, "*all*") {
		return true
	}

	// Do a case-insensitive check
	for _, item := range ignore {
		if strings.EqualFold(item, name) {
			return true
		}
	}
	return false
}

// parser
type DefaultHCLParser struct{}

func (p *DefaultHCLParser) ParseProviderRequirements(filename string) (map[string]ProviderConfig, error) {
	parser := hclparse.NewParser()
	if filename == "" {
		return map[string]ProviderConfig{}, nil
	}

	_, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return map[string]ProviderConfig{}, nil
	}

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
		if blk == nil || blk.Body == nil {
			continue
		}

		if blk.Type == "terraform" {
			for _, innerBlk := range blk.Body.Blocks {
				if innerBlk == nil || innerBlk.Body == nil {
					continue
				}

				if innerBlk.Type == "required_providers" {
					attrs, _ := innerBlk.Body.JustAttributes()
					for name, attr := range attrs {
						if attr == nil || attr.Expr == nil {
							continue
						}

						val, valDiags := attr.Expr.Value(nil)
						if valDiags != nil && valDiags.HasErrors() {
							continue
						}

						if val.Type().IsObjectType() {
							pc := ProviderConfig{}
							sourceVal := val.GetAttr("source")
							if !sourceVal.IsNull() {
								pc.Source = normalizeSource(sourceVal.AsString())
							}
							versionVal := val.GetAttr("version")
							if !versionVal.IsNull() {
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
	if filename == "" {
		return nil, nil, fmt.Errorf("empty filename provided")
	}

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
		if blk == nil || blk.Body == nil {
			continue
		}

		// Parse resources
		if blk.Type == "resource" && len(blk.Labels) >= 2 {
			parsed := ParseSyntaxBody(blk.Body)

			// Direct AST extraction of ignore_changes as a backup
			ignoreChanges := extractLifecycleIgnoreChangesFromAST(blk.Body)
			if len(ignoreChanges) > 0 {
				parsed.data.ignoreChanges = append(parsed.data.ignoreChanges, ignoreChanges...)
			}

			res := ParsedResource{
				Type: blk.Labels[0],
				Name: blk.Labels[1],
				data: parsed.data,
			}
			resources = append(resources, res)
		}

		// Parse data sources
		if blk.Type == "data" && len(blk.Labels) >= 2 {
			parsed := ParseSyntaxBody(blk.Body)

			// Direct AST extraction for data sources too
			ignoreChanges := extractLifecycleIgnoreChangesFromAST(blk.Body)
			if len(ignoreChanges) > 0 {
				parsed.data.ignoreChanges = append(parsed.data.ignoreChanges, ignoreChanges...)
			}

			ds := ParsedDataSource{
				Type: blk.Labels[0],
				Name: blk.Labels[1],
				data: parsed.data,
			}
			dataSources = append(dataSources, ds)
		}
	}
	return resources, dataSources, nil
}

// github issues
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

	// Deduplicate exact lines in the GitHub issue (just to avoid repeating the same line).
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
		entityType := "resource"
		if f.IsDataSource {
			entityType = "data source"
		}

		if f.SubmoduleName == "" {
			fmt.Fprintf(&newBody, "`%s`: missing %s %s `%s` in `%s` (%s)\n\n",
				f.ResourceType, status, itemType, f.Name, cleanPath, entityType,
			)
		} else {
			fmt.Fprintf(&newBody, "`%s`: missing %s %s `%s` in `%s` in submodule `%s` (%s)\n\n",
				f.ResourceType, status, itemType, f.Name, cleanPath, f.SubmoduleName, entityType,
			)
		}
	}

	title := "Generated schema validation"
	issueNum, existingBody, err := g.findExistingIssue(title)
	if err != nil {
		return fmt.Errorf("error finding existing issue: %w", err)
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
	if g.Client == nil {
		return 0, "", fmt.Errorf("HTTP client is nil")
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues?state=open", g.RepoOwner, g.RepoName)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "token "+g.token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := g.Client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("failed to send request: %w", err)
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
		return 0, "", fmt.Errorf("failed to decode response: %w", err)
	}

	for _, issue := range issues {
		if issue.Title == title {
			return issue.Number, issue.Body, nil
		}
	}
	return 0, "", nil
}

func (g *GitHubIssueService) updateIssue(issueNumber int, body string) error {
	if g.Client == nil {
		return fmt.Errorf("HTTP client is nil")
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d", g.RepoOwner, g.RepoName, issueNumber)
	payload := struct {
		Body string `json:"body"`
	}{Body: body}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequest("PATCH", url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "token "+g.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.Client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("GitHub API error: %s", resp.Status)
	}

	return nil
}

func (g *GitHubIssueService) createIssue(title, body string) error {
	if g.Client == nil {
		return fmt.Errorf("HTTP client is nil")
	}

	payload := struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}{
		Title: title,
		Body:  body,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues", g.RepoOwner, g.RepoName)
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "token "+g.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.Client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("GitHub API error: %s", resp.Status)
	}

	return nil
}

// repository info
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
			owner, repo = parts[0], parts[1]
			return owner, repo
		}
	}
	return "", ""
}

func TestValidateTerraformSchema(t *testing.T) {
	// root directory from env or "."
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
	if rootFindings != nil {
		allFindings = append(allFindings, rootFindings...)
	}

	// Validate submodules in modules/<n>/ (one level)
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
		if f != nil {
			allFindings = append(allFindings, f...)
		}
	}

	// Deduplicate findings before logging
	deduplicatedFindings := deduplicateFindings(allFindings)

	// Log all missing
	for _, f := range deduplicatedFindings {
		place := "root"
		if f.SubmoduleName != "" {
			place = "root in submodule " + f.SubmoduleName
		}
		requiredOptional := boolToStr(f.Required, "required", "optional")
		blockOrProp := boolToStr(f.IsBlock, "block", "property")
		entityType := "resource"
		if f.IsDataSource {
			entityType = "data source"
		}
		t.Logf("%s missing %s %s %q in %s (%s)", f.ResourceType, requiredOptional, blockOrProp, f.Name, place, entityType)
	}

	// If GITHUB_TOKEN is set, create/update single GH issue
	if ghToken := os.Getenv("GITHUB_TOKEN"); ghToken != "" {
		if len(deduplicatedFindings) > 0 {
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
	}

	// FAIL if ANY missing items (required or optional) exist:
	if len(deduplicatedFindings) > 0 {
		t.Fatalf("Found %d missing properties/blocks in root or submodules. See logs above.", len(deduplicatedFindings))
	}
}

func deduplicateFindings(findings []ValidationFinding) []ValidationFinding {
	if len(findings) == 0 {
		return nil
	}

	// Create a map to detect duplicates
	seen := make(map[string]bool)
	var result []ValidationFinding

	for _, f := range findings {
		// Create a unique key for each finding
		key := fmt.Sprintf("%s|%s|%s|%v|%v|%s",
			f.ResourceType,
			f.Path,
			f.Name,
			f.IsBlock,
			f.IsDataSource,
			f.SubmoduleName,
		)

		// Only add to the result if we haven't seen this exact finding before
		if !seen[key] {
			seen[key] = true
			result = append(result, f)
		}
	}

	return result
}

func validateTerraformSchemaInDir(t *testing.T, dir, submoduleName string) ([]ValidationFinding, error) {
	if dir == "" {
		return nil, fmt.Errorf("empty directory provided")
	}

	mainTf := filepath.Join(dir, "main.tf")
	if _, err := os.Stat(mainTf); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("error checking main.tf: %w", err)
	}

	parser := &DefaultHCLParser{}
	tfFile := filepath.Join(dir, "terraform.tf")
	providers, err := parser.ParseProviderRequirements(tfFile)
	if err != nil {
		return nil, fmt.Errorf("failed to parse provider config in %s: %w", dir, err)
	}

	// cleanup
	defer func() {
		// Ignore errors on cleanup operations
		_ = os.RemoveAll(filepath.Join(dir, ".terraform"))
		_ = os.Remove(filepath.Join(dir, "terraform.tfstate"))
		_ = os.Remove(filepath.Join(dir, ".terraform.lock.hcl"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	initCmd := exec.CommandContext(ctx, "terraform", "init")
	initCmd.Dir = dir
	out, err := initCmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("terraform init failed in %s: %v\nOutput: %s", dir, err, string(out))
	}

	schemaCmd := exec.CommandContext(ctx, "terraform", "providers", "schema", "-json")
	schemaCmd.Dir = dir
	out, err = schemaCmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		stderr := ""
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		return nil, fmt.Errorf("failed to get schema in %s: %w\nStderr: %s", dir, err, stderr)
	}

	var tfSchema TerraformSchema
	if err := json.Unmarshal(out, &tfSchema); err != nil {
		return nil, fmt.Errorf("failed to unmarshal schema in %s: %w", dir, err)
	}

	resources, dataSources, err := parser.ParseMainFile(mainTf)
	if err != nil {
		return nil, fmt.Errorf("parseMainFile in %s: %w", dir, err)
	}

	// compare resources
	var findings []ValidationFinding

	// Process resources
	for _, r := range resources {
		// e.g. "azurerm_virtual_hub_routing_intent" => provider name "azurerm"
		parts := strings.SplitN(r.Type, "_", 2)
		if len(parts) < 2 {
			t.Logf("Invalid resource type format: %s", r.Type)
			continue
		}

		provName := parts[0]
		cfg, ok := providers[provName]
		if !ok {
			t.Logf("No provider config for resource type %s in %s", r.Type, dir)
			continue
		}

		pSchema, ok := tfSchema.ProviderSchemas[cfg.Source]
		if !ok || pSchema == nil {
			t.Logf("No provider schema found for source %s in %s", cfg.Source, dir)
			continue
		}

		resSchema, ok := pSchema.ResourceSchemas[r.Type]
		if !ok || resSchema == nil || resSchema.Block == nil {
			t.Logf("No resource schema found for %s in provider %s (dir=%s)", r.Type, cfg.Source, dir)
			continue
		}

		var local []ValidationFinding
		// Pass resource-level ignoreChanges to initial validation
		r.data.Validate(t, r.Type, "root", resSchema.Block, r.data.ignoreChanges, &local)

		// Filter out any findings for attributes that are in ignore_changes (case-insensitively)
		for i := range local {
			// Check if the finding should be excluded due to ignore_changes
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

	// Process data sources
	for _, ds := range dataSources {
		parts := strings.SplitN(ds.Type, "_", 2)
		if len(parts) < 2 {
			t.Logf("Invalid data source type format: %s", ds.Type)
			continue
		}

		provName := parts[0]
		cfg, ok := providers[provName]
		if !ok {
			t.Logf("No provider config for data source type %s in %s", ds.Type, dir)
			continue
		}

		pSchema, ok := tfSchema.ProviderSchemas[cfg.Source]
		if !ok || pSchema == nil {
			t.Logf("No provider schema found for source %s in %s", cfg.Source, dir)
			continue
		}

		dsSchema, ok := pSchema.DataSourceSchemas[ds.Type]
		if !ok || dsSchema == nil || dsSchema.Block == nil {
			t.Logf("No data source schema found for %s in provider %s (dir=%s)", ds.Type, cfg.Source, dir)
			continue
		}

		var local []ValidationFinding
		// Pass data source-level ignoreChanges to initial validation
		ds.data.Validate(t, ds.Type, "root", dsSchema.Block, ds.data.ignoreChanges, &local)

		// Filter out any findings that should be excluded (case-insensitively)
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
				local[i].IsDataSource = true // Mark as data source
				findings = append(findings, local[i])
			}
		}
	}

	return findings, nil
}

// findSubmodules => subdirectories under modulesDir (one level) that contain main.tf
func findSubmodules(modulesDir string) ([]struct {
	name string
	path string
}, error) {
	var result []struct {
		name string
		path string
	}

	if modulesDir == "" {
		return result, nil
	}

	entries, err := os.ReadDir(modulesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return result, fmt.Errorf("failed to read modules directory: %w", err)
	}

	for _, e := range entries {
		if e.IsDir() {
			subName := e.Name()
			subPath := filepath.Join(modulesDir, subName)
			mainTf := filepath.Join(subPath, "main.tf")
			if _, err := os.Stat(mainTf); err == nil {
				result = append(result, struct {
					name string
					path string
				}{subName, subPath})
			}
		}
	}
	return result, nil
}

func boolToStr(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}

func normalizeSource(source string) string {
	if source == "" {
		return ""
	}

	// e.g. "hashicorp/azurerm" => "registry.terraform.io/hashicorp/azurerm"
	if strings.Contains(source, "/") && !strings.Contains(source, "registry.terraform.io/") {
		return "registry.terraform.io/" + source
	}
	return source
}

// Merges dynamic blocks, ignoring etc
func mergeBlocks(dest, src *ParsedBlock) {
	if dest == nil || src == nil {
		return
	}

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

func extractIgnoreChanges(val cty.Value) []string {
	var changes []string

	if !val.IsKnown() || val.IsNull() {
		return changes
	}

	if val.Type().IsCollectionType() {
		for it := val.ElementIterator(); it.Next(); {
			_, v := it.Element()
			if v.Type() == cty.String {
				str := v.AsString()
				// If "all" is specified, return a special marker
				if str == "all" {
					return []string{"*all*"}
				}
				changes = append(changes, str)
			}
		}
	}
	return changes
}

func findContentBlock(body *hclsyntax.Body) *hclsyntax.Body {
	if body == nil {
		return nil
	}

	for _, b := range body.Blocks {
		if b != nil && b.Type == "content" && b.Body != nil {
			return b.Body
		}
	}
	return body
}

func ParseSyntaxBody(body *hclsyntax.Body) *ParsedBlock {
	if body == nil {
		return &ParsedBlock{data: NewBlockData()}
	}

	bd := NewBlockData()
	blk := &ParsedBlock{data: bd}
	bd.ParseAttributes(body)
	bd.ParseBlocks(body)
	return blk
}

//TODO: fix warnings
// package main
//
// import (
// 	"bytes"
// 	"context"
// 	"encoding/json"
// 	"errors"
// 	"fmt"
// 	"github.com/hashicorp/hcl/v2/hclparse"
// 	"github.com/hashicorp/hcl/v2/hclsyntax"
// 	"github.com/zclconf/go-cty/cty"
// 	"golang.org/x/exp/slices"
// 	"net/http"
// 	"os"
// 	"os/exec"
// 	"path/filepath"
// 	"strings"
// 	"sync"
// 	"testing"
// 	"time"
// )
//
// type BlockProcessor interface {
// 	ParseAttributes(body *hclsyntax.Body)
// 	ParseBlocks(body *hclsyntax.Body)
// 	Validate(t *testing.T, resourceType, path string, schema *SchemaBlock, parentIgnore []string, findings *[]ValidationFinding)
// }
//
// type HCLParser interface {
// 	ParseProviderRequirements(filename string) (map[string]ProviderConfig, error)
// 	ParseMainFile(filename string) ([]ParsedResource, []ParsedDataSource, error)
// }
//
// type IssueManager interface {
// 	CreateOrUpdateIssue(findings []ValidationFinding) error
// }
//
// type RepositoryInfoProvider interface {
// 	GetRepoInfo() (owner, name string)
// }
//
// // schema definitions
// type TerraformSchema struct {
// 	ProviderSchemas map[string]*ProviderSchema `json:"provider_schemas"`
// }
//
// type ProviderSchema struct {
// 	ResourceSchemas   map[string]*ResourceSchema `json:"resource_schemas"`
// 	DataSourceSchemas map[string]*ResourceSchema `json:"data_source_schemas"`
// }
//
// type ResourceSchema struct {
// 	Block *SchemaBlock `json:"block"`
// }
//
// type SchemaBlock struct {
// 	Attributes map[string]*SchemaAttribute `json:"attributes"`
// 	BlockTypes map[string]*SchemaBlockType `json:"block_types"`
// }
//
// type SchemaAttribute struct {
// 	Required bool `json:"required"`
// 	Optional bool `json:"optional"`
// 	Computed bool `json:"computed"`
// }
//
// type SchemaBlockType struct {
// 	Nesting  string       `json:"nesting"`
// 	MinItems int          `json:"min_items"`
// 	MaxItems int          `json:"max_items"`
// 	Block    *SchemaBlock `json:"block"`
// }
//
// // ValidationFinding logs any missing attribute/block.
// type ValidationFinding struct {
// 	ResourceType  string
// 	Path          string // e.g., "root" or "root.some_nested_block"
// 	Name          string
// 	Required      bool
// 	IsBlock       bool
// 	IsDataSource  bool   // If true, this is a data source, not a resource
// 	SubmoduleName string // empty => root, else submodule name
// }
//
// type ProviderConfig struct {
// 	Source  string
// 	Version string
// }
//
// type ParsedResource struct {
// 	Type string
// 	Name string
// 	data BlockData
// }
//
// type ParsedDataSource struct {
// 	Type string
// 	Name string
// 	data BlockData
// }
//
// type BlockData struct {
// 	properties    map[string]bool
// 	staticBlocks  map[string]*ParsedBlock
// 	dynamicBlocks map[string]*ParsedBlock
// 	ignoreChanges []string
// }
//
// type ParsedBlock struct {
// 	data BlockData
// }
//
// // Added mutex for concurrent access protection
// type GlobalMessagesStore struct {
// 	sync.Mutex
// 	messages map[string]bool
// }
//
// func NewGlobalMessagesStore() *GlobalMessagesStore {
// 	return &GlobalMessagesStore{
// 		messages: make(map[string]bool),
// 	}
// }
//
// func (s *GlobalMessagesStore) Add(key string) {
// 	s.Lock()
// 	defer s.Unlock()
// 	s.messages[key] = true
// }
//
// func (s *GlobalMessagesStore) GetAllMessages() []string {
// 	s.Lock()
// 	defer s.Unlock()
//
// 	var result []string
// 	for msg := range s.messages {
// 		result = append(result, msg)
// 	}
// 	return result
// }
//
// func NewBlockData() BlockData {
// 	return BlockData{
// 		properties:    make(map[string]bool),
// 		staticBlocks:  make(map[string]*ParsedBlock),
// 		dynamicBlocks: make(map[string]*ParsedBlock),
// 		ignoreChanges: []string{},
// 	}
// }
//
// // Extract ignore_changes directly from AST
// func extractLifecycleIgnoreChangesFromAST(body *hclsyntax.Body) []string {
// 	if body == nil {
// 		return nil
// 	}
//
// 	var ignoreChanges []string
//
// 	for _, block := range body.Blocks {
// 		if block == nil || block.Body == nil {
// 			continue
// 		}
//
// 		if block.Type == "lifecycle" {
// 			for name, attr := range block.Body.Attributes {
// 				if name == "ignore_changes" && attr != nil && attr.Expr != nil {
// 					if listExpr, ok := attr.Expr.(*hclsyntax.TupleConsExpr); ok {
// 						for _, expr := range listExpr.Exprs {
// 							if expr == nil {
// 								continue
// 							}
//
// 							if traversalExpr, ok := expr.(*hclsyntax.ScopeTraversalExpr); ok {
// 								if len(traversalExpr.Traversal) > 0 {
// 									ignoreChanges = append(ignoreChanges, traversalExpr.Traversal.RootName())
// 								}
// 							} else if templateExpr, ok := expr.(*hclsyntax.TemplateExpr); ok {
// 								if len(templateExpr.Parts) == 1 && templateExpr.Parts[0] != nil {
// 									if literalPart, ok := templateExpr.Parts[0].(*hclsyntax.LiteralValueExpr); ok {
// 										if literalPart.Val.Type() == cty.String {
// 											ignoreChanges = append(ignoreChanges, literalPart.Val.AsString())
// 										}
// 									}
// 								}
// 							} else if literalExpr, ok := expr.(*hclsyntax.LiteralValueExpr); ok {
// 								if literalExpr.Val.Type() == cty.String {
// 									ignoreChanges = append(ignoreChanges, literalExpr.Val.AsString())
// 								}
// 							}
// 						}
// 					}
// 				}
// 			}
// 		}
// 	}
//
// 	return ignoreChanges
// }
//
// func (bd *BlockData) ParseAttributes(body *hclsyntax.Body) {
// 	if body == nil {
// 		return
// 	}
//
// 	for name := range body.Attributes {
// 		bd.properties[name] = true
// 	}
// }
//
// func (bd *BlockData) ParseBlocks(body *hclsyntax.Body) {
// 	if body == nil {
// 		return
// 	}
//
// 	// First, directly extract any lifecycle ignore_changes from AST
// 	directIgnoreChanges := extractLifecycleIgnoreChangesFromAST(body)
// 	if len(directIgnoreChanges) > 0 {
// 		bd.ignoreChanges = append(bd.ignoreChanges, directIgnoreChanges...)
// 	}
//
// 	for _, block := range body.Blocks {
// 		if block == nil {
// 			continue
// 		}
//
// 		switch block.Type {
// 		case "lifecycle":
// 			bd.parseLifecycle(block.Body)
// 		case "dynamic":
// 			if len(block.Labels) == 1 {
// 				bd.parseDynamicBlock(block.Body, block.Labels[0])
// 			}
// 		default:
// 			parsed := ParseSyntaxBody(block.Body)
// 			bd.staticBlocks[block.Type] = parsed
// 		}
// 	}
// }
//
// func (bd *BlockData) Validate(
// 	t *testing.T,
// 	resourceType, path string,
// 	schema *SchemaBlock,
// 	parentIgnore []string,
// 	findings *[]ValidationFinding,
// ) {
// 	if schema == nil || findings == nil {
// 		return
// 	}
// 	// Combine parent ignore changes with this block's ignore changes
// 	ignore := make([]string, 0, len(parentIgnore)+len(bd.ignoreChanges))
// 	ignore = append(ignore, parentIgnore...)
// 	ignore = append(ignore, bd.ignoreChanges...)
//
// 	bd.validateAttributes(t, resourceType, path, schema, ignore, findings)
// 	bd.validateBlocks(t, resourceType, path, schema, ignore, findings)
// }
//
// // parseLifecycle picks up ignore_changes in lifecycle { }
// func (bd *BlockData) parseLifecycle(body *hclsyntax.Body) {
// 	if body == nil {
// 		return
// 	}
//
// 	for name, attr := range body.Attributes {
// 		if name == "ignore_changes" && attr != nil && attr.Expr != nil {
// 			val, diags := attr.Expr.Value(nil)
// 			// Only process if we could get a value without errors
// 			if diags == nil || !diags.HasErrors() {
// 				extracted := extractIgnoreChanges(val)
// 				bd.ignoreChanges = append(bd.ignoreChanges, extracted...)
// 			}
// 		}
// 	}
// }
//
// func (bd *BlockData) parseDynamicBlock(body *hclsyntax.Body, name string) {
// 	if body == nil || name == "" {
// 		return
// 	}
//
// 	contentBlock := findContentBlock(body)
// 	parsed := ParseSyntaxBody(contentBlock)
// 	if existing := bd.dynamicBlocks[name]; existing != nil {
// 		mergeBlocks(existing, parsed)
// 	} else {
// 		bd.dynamicBlocks[name] = parsed
// 	}
// }
//
// func (bd *BlockData) validateAttributes(
// 	t *testing.T,
// 	resType, path string,
// 	schema *SchemaBlock,
// 	ignore []string,
// 	findings *[]ValidationFinding,
// ) {
// 	if schema == nil || schema.Attributes == nil || findings == nil {
// 		return
// 	}
//
// 	for name, attr := range schema.Attributes {
// 		if attr == nil {
// 			continue
// 		}
//
// 		// Skip 'id' property as it's not useful to show
// 		if name == "id" {
// 			continue
// 		}
//
// 		// Skip purely computed attributes (those that are computed but not optional)
// 		// These are always exported, never set by the user
// 		if attr.Computed && !attr.Optional && !attr.Required {
// 			continue
// 		}
//
// 		// Skip attributes in the ignore list (case insensitive)
// 		if isIgnored(ignore, name) {
// 			continue
// 		}
//
// 		if !bd.properties[name] {
// 			*findings = append(*findings, ValidationFinding{
// 				ResourceType: resType,
// 				Path:         path,
// 				Name:         name,
// 				Required:     attr.Required,
// 				IsBlock:      false,
// 			})
// 			// No direct logging here to avoid duplication
// 		}
// 	}
// }
//
// func (bd *BlockData) validateBlocks(
// 	t *testing.T,
// 	resType, path string,
// 	schema *SchemaBlock,
// 	ignore []string,
// 	findings *[]ValidationFinding,
// ) {
// 	if schema == nil || schema.BlockTypes == nil || findings == nil {
// 		return
// 	}
//
// 	for name, blockType := range schema.BlockTypes {
// 		if blockType == nil {
// 			continue
// 		}
//
// 		// skip timeouts or ignored blocks (case insensitive)
// 		if name == "timeouts" || isIgnored(ignore, name) {
// 			continue
// 		}
// 		static := bd.staticBlocks[name]
// 		dynamic := bd.dynamicBlocks[name]
// 		if static == nil && dynamic == nil {
// 			*findings = append(*findings, ValidationFinding{
// 				ResourceType: resType,
// 				Path:         path,
// 				Name:         name,
// 				Required:     blockType.MinItems > 0,
// 				IsBlock:      true,
// 			})
// 			// No direct logging here to avoid duplication
// 			continue
// 		}
// 		var target *ParsedBlock
// 		if static != nil {
// 			target = static
// 		} else {
// 			target = dynamic
// 		}
// 		newPath := fmt.Sprintf("%s.%s", path, name)
// 		target.data.Validate(t, resType, newPath, blockType.Block, ignore, findings)
// 	}
// }
//
// // Helper function to check if an item is ignored (case insensitive)
// func isIgnored(ignore []string, name string) bool {
// 	if name == "" {
// 		return false
// 	}
//
// 	// Check for the special "all" marker
// 	if slices.Contains(ignore, "*all*") {
// 		return true
// 	}
//
// 	// Do a case-insensitive check
// 	for _, item := range ignore {
// 		if strings.EqualFold(item, name) {
// 			return true
// 		}
// 	}
// 	return false
// }
//
// // parser
// type DefaultHCLParser struct{}
//
// func (p *DefaultHCLParser) ParseProviderRequirements(filename string) (map[string]ProviderConfig, error) {
// 	parser := hclparse.NewParser()
// 	if filename == "" {
// 		return map[string]ProviderConfig{}, nil
// 	}
//
// 	_, err := os.Stat(filename)
// 	if os.IsNotExist(err) {
// 		return map[string]ProviderConfig{}, nil
// 	}
//
// 	f, diags := parser.ParseHCLFile(filename)
// 	if diags.HasErrors() {
// 		return nil, fmt.Errorf("parse error in file %s: %v", filename, diags)
// 	}
// 	body, ok := f.Body.(*hclsyntax.Body)
// 	if !ok {
// 		return nil, fmt.Errorf("invalid body in file %s", filename)
// 	}
// 	providers := make(map[string]ProviderConfig)
// 	for _, blk := range body.Blocks {
// 		if blk == nil || blk.Body == nil {
// 			continue
// 		}
//
// 		if blk.Type == "terraform" {
// 			for _, innerBlk := range blk.Body.Blocks {
// 				if innerBlk == nil || innerBlk.Body == nil {
// 					continue
// 				}
//
// 				if innerBlk.Type == "required_providers" {
// 					attrs, _ := innerBlk.Body.JustAttributes()
// 					for name, attr := range attrs {
// 						if attr == nil || attr.Expr == nil {
// 							continue
// 						}
//
// 						val, valDiags := attr.Expr.Value(nil)
// 						if valDiags != nil && valDiags.HasErrors() {
// 							continue
// 						}
//
// 						if val.Type().IsObjectType() {
// 							pc := ProviderConfig{}
// 							sourceVal := val.GetAttr("source")
// 							if !sourceVal.IsNull() {
// 								pc.Source = normalizeSource(sourceVal.AsString())
// 							}
// 							versionVal := val.GetAttr("version")
// 							if !versionVal.IsNull() {
// 								pc.Version = versionVal.AsString()
// 							}
// 							providers[name] = pc
// 						}
// 					}
// 				}
// 			}
// 		}
// 	}
// 	return providers, nil
// }
//
// func (p *DefaultHCLParser) ParseMainFile(filename string) ([]ParsedResource, []ParsedDataSource, error) {
// 	if filename == "" {
// 		return nil, nil, fmt.Errorf("empty filename provided")
// 	}
//
// 	parser := hclparse.NewParser()
// 	f, diags := parser.ParseHCLFile(filename)
// 	if diags.HasErrors() {
// 		return nil, nil, fmt.Errorf("parse error in file %s: %v", filename, diags)
// 	}
// 	body, ok := f.Body.(*hclsyntax.Body)
// 	if !ok {
// 		return nil, nil, fmt.Errorf("invalid body in file %s", filename)
// 	}
// 	var resources []ParsedResource
// 	var dataSources []ParsedDataSource
//
// 	for _, blk := range body.Blocks {
// 		if blk == nil || blk.Body == nil {
// 			continue
// 		}
//
// 		// Parse resources
// 		if blk.Type == "resource" && len(blk.Labels) >= 2 {
// 			parsed := ParseSyntaxBody(blk.Body)
//
// 			// Direct AST extraction of ignore_changes as a backup
// 			ignoreChanges := extractLifecycleIgnoreChangesFromAST(blk.Body)
// 			if len(ignoreChanges) > 0 {
// 				parsed.data.ignoreChanges = append(parsed.data.ignoreChanges, ignoreChanges...)
// 			}
//
// 			res := ParsedResource{
// 				Type: blk.Labels[0],
// 				Name: blk.Labels[1],
// 				data: parsed.data,
// 			}
// 			resources = append(resources, res)
// 		}
//
// 		// Parse data sources
// 		if blk.Type == "data" && len(blk.Labels) >= 2 {
// 			parsed := ParseSyntaxBody(blk.Body)
//
// 			// Direct AST extraction for data sources too
// 			ignoreChanges := extractLifecycleIgnoreChangesFromAST(blk.Body)
// 			if len(ignoreChanges) > 0 {
// 				parsed.data.ignoreChanges = append(parsed.data.ignoreChanges, ignoreChanges...)
// 			}
//
// 			ds := ParsedDataSource{
// 				Type: blk.Labels[0],
// 				Name: blk.Labels[1],
// 				data: parsed.data,
// 			}
// 			dataSources = append(dataSources, ds)
// 		}
// 	}
// 	return resources, dataSources, nil
// }
//
// // github issues
// type GitHubIssueService struct {
// 	RepoOwner string
// 	RepoName  string
// 	token     string
// 	Client    *http.Client
// }
//
// func (g *GitHubIssueService) CreateOrUpdateIssue(findings []ValidationFinding) error {
// 	if len(findings) == 0 {
// 		return nil
// 	}
//
// 	const header = "### \n\n"
// 	dedup := make(map[string]ValidationFinding)
//
// 	// Deduplicate exact lines in the GitHub issue (just to avoid repeating the same line).
// 	for _, f := range findings {
// 		key := fmt.Sprintf("%s|%s|%s|%v|%v|%s",
// 			f.ResourceType,
// 			strings.ReplaceAll(f.Path, "root.", ""),
// 			f.Name,
// 			f.IsBlock,
// 			f.IsDataSource,
// 			f.SubmoduleName,
// 		)
// 		dedup[key] = f
// 	}
//
// 	var newBody bytes.Buffer
// 	fmt.Fprint(&newBody, header)
// 	for _, f := range dedup {
// 		cleanPath := strings.ReplaceAll(f.Path, "root.", "")
// 		status := boolToStr(f.Required, "required", "optional")
// 		itemType := boolToStr(f.IsBlock, "block", "property")
// 		entityType := "resource"
// 		if f.IsDataSource {
// 			entityType = "data source"
// 		}
//
// 		if f.SubmoduleName == "" {
// 			fmt.Fprintf(&newBody, "`%s`: missing %s %s `%s` in `%s` (%s)\n\n",
// 				f.ResourceType, status, itemType, f.Name, cleanPath, entityType,
// 			)
// 		} else {
// 			fmt.Fprintf(&newBody, "`%s`: missing %s %s `%s` in `%s` in submodule `%s` (%s)\n\n",
// 				f.ResourceType, status, itemType, f.Name, cleanPath, f.SubmoduleName, entityType,
// 			)
// 		}
// 	}
//
// 	title := "Generated schema validation"
// 	issueNum, existingBody, err := g.findExistingIssue(title)
// 	if err != nil {
// 		return fmt.Errorf("error finding existing issue: %w", err)
// 	}
//
// 	finalBody := newBody.String()
// 	if issueNum > 0 {
// 		parts := strings.SplitN(existingBody, header, 2)
// 		if len(parts) > 0 {
// 			finalBody = strings.TrimSpace(parts[0]) + "\n\n" + newBody.String()
// 		}
// 		return g.updateIssue(issueNum, finalBody)
// 	}
// 	return g.createIssue(title, finalBody)
// }
//
// func (g *GitHubIssueService) findExistingIssue(title string) (int, string, error) {
// 	if g.Client == nil {
// 		return 0, "", fmt.Errorf("HTTP client is nil")
// 	}
//
// 	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues?state=open", g.RepoOwner, g.RepoName)
// 	req, err := http.NewRequest("GET", url, nil)
// 	if err != nil {
// 		return 0, "", fmt.Errorf("failed to create request: %w", err)
// 	}
//
// 	req.Header.Set("Authorization", "token "+g.token)
// 	req.Header.Set("Accept", "application/vnd.github.v3+json")
//
// 	resp, err := g.Client.Do(req)
// 	if err != nil {
// 		return 0, "", fmt.Errorf("failed to send request: %w", err)
// 	}
// 	defer resp.Body.Close()
//
// 	if resp.StatusCode != http.StatusOK {
// 		return 0, "", fmt.Errorf("GitHub API error: %s", resp.Status)
// 	}
//
// 	var issues []struct {
// 		Number int    `json:"number"`
// 		Title  string `json:"title"`
// 		Body   string `json:"body"`
// 	}
// 	if err := json.NewDecoder(resp.Body).Decode(&issues); err != nil {
// 		return 0, "", fmt.Errorf("failed to decode response: %w", err)
// 	}
//
// 	for _, issue := range issues {
// 		if issue.Title == title {
// 			return issue.Number, issue.Body, nil
// 		}
// 	}
// 	return 0, "", nil
// }
//
// func (g *GitHubIssueService) updateIssue(issueNumber int, body string) error {
// 	if g.Client == nil {
// 		return fmt.Errorf("HTTP client is nil")
// 	}
//
// 	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d", g.RepoOwner, g.RepoName, issueNumber)
// 	payload := struct {
// 		Body string `json:"body"`
// 	}{Body: body}
//
// 	data, err := json.Marshal(payload)
// 	if err != nil {
// 		return fmt.Errorf("failed to marshal payload: %w", err)
// 	}
//
// 	req, err := http.NewRequest("PATCH", url, bytes.NewReader(data))
// 	if err != nil {
// 		return fmt.Errorf("failed to create request: %w", err)
// 	}
//
// 	req.Header.Set("Authorization", "token "+g.token)
// 	req.Header.Set("Content-Type", "application/json")
//
// 	resp, err := g.Client.Do(req)
// 	if err != nil {
// 		return fmt.Errorf("failed to send request: %w", err)
// 	}
// 	defer resp.Body.Close()
//
// 	if resp.StatusCode >= 400 {
// 		return fmt.Errorf("GitHub API error: %s", resp.Status)
// 	}
//
// 	return nil
// }
//
// func (g *GitHubIssueService) createIssue(title, body string) error {
// 	if g.Client == nil {
// 		return fmt.Errorf("HTTP client is nil")
// 	}
//
// 	payload := struct {
// 		Title string `json:"title"`
// 		Body  string `json:"body"`
// 	}{
// 		Title: title,
// 		Body:  body,
// 	}
//
// 	data, err := json.Marshal(payload)
// 	if err != nil {
// 		return fmt.Errorf("failed to marshal payload: %w", err)
// 	}
//
// 	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues", g.RepoOwner, g.RepoName)
// 	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
// 	if err != nil {
// 		return fmt.Errorf("failed to create request: %w", err)
// 	}
//
// 	req.Header.Set("Authorization", "token "+g.token)
// 	req.Header.Set("Content-Type", "application/json")
//
// 	resp, err := g.Client.Do(req)
// 	if err != nil {
// 		return fmt.Errorf("failed to send request: %w", err)
// 	}
// 	defer resp.Body.Close()
//
// 	if resp.StatusCode >= 400 {
// 		return fmt.Errorf("GitHub API error: %s", resp.Status)
// 	}
//
// 	return nil
// }
//
// // repository info
// type GitRepoInfo struct {
// 	terraformRoot string
// }
//
// func (g *GitRepoInfo) GetRepoInfo() (owner, repo string) {
// 	owner = os.Getenv("GITHUB_REPOSITORY_OWNER")
// 	repo = os.Getenv("GITHUB_REPOSITORY_NAME")
// 	if owner != "" && repo != "" {
// 		return owner, repo
// 	}
//
// 	if ghRepo := os.Getenv("GITHUB_REPOSITORY"); ghRepo != "" {
// 		parts := strings.SplitN(ghRepo, "/", 2)
// 		if len(parts) == 2 {
// 			owner, repo = parts[0], parts[1]
// 			return owner, repo
// 		}
// 	}
// 	return "", ""
// }
//
// func TestValidateTerraformSchema(t *testing.T) {
// 	// root directory from env or "."
// 	terraformRoot := os.Getenv("TERRAFORM_ROOT")
// 	if terraformRoot == "" {
// 		terraformRoot = "."
// 	}
//
// 	// Validate root
// 	rootFindings, err := validateTerraformSchemaInDir(t, terraformRoot, "")
// 	if err != nil {
// 		t.Fatalf("Failed to validate root at %s: %v", terraformRoot, err)
// 	}
// 	var allFindings []ValidationFinding
// 	if rootFindings != nil {
// 		allFindings = append(allFindings, rootFindings...)
// 	}
//
// 	// Validate submodules in modules/<n>/ (one level)
// 	modulesDir := filepath.Join(terraformRoot, "modules")
// 	subs, err := findSubmodules(modulesDir)
// 	if err != nil {
// 		t.Fatalf("Failed to find submodules in %s: %v", modulesDir, err)
// 	}
// 	for _, sm := range subs {
// 		f, sErr := validateTerraformSchemaInDir(t, sm.path, sm.name)
// 		if sErr != nil {
// 			t.Errorf("Failed to validate submodule %s: %v", sm.name, sErr)
// 			continue
// 		}
// 		if f != nil {
// 			allFindings = append(allFindings, f...)
// 		}
// 	}
//
// 	// Deduplicate findings before logging
// 	deduplicatedFindings := deduplicateFindings(allFindings)
//
// 	// Log all missing
// 	for _, f := range deduplicatedFindings {
// 		place := "root"
// 		if f.SubmoduleName != "" {
// 			place = "root in submodule " + f.SubmoduleName
// 		}
// 		requiredOptional := boolToStr(f.Required, "required", "optional")
// 		blockOrProp := boolToStr(f.IsBlock, "block", "property")
// 		entityType := "resource"
// 		if f.IsDataSource {
// 			entityType = "data source"
// 		}
// 		t.Logf("%s missing %s %s %q in %s (%s)", f.ResourceType, requiredOptional, blockOrProp, f.Name, place, entityType)
// 	}
//
// 	// If GITHUB_TOKEN is set, create/update single GH issue
// 	if ghToken := os.Getenv("GITHUB_TOKEN"); ghToken != "" {
// 		if len(deduplicatedFindings) > 0 {
// 			gi := &GitRepoInfo{terraformRoot: terraformRoot}
// 			owner, repoName := gi.GetRepoInfo()
// 			if owner != "" && repoName != "" {
// 				gh := &GitHubIssueService{
// 					RepoOwner: owner,
// 					RepoName:  repoName,
// 					token:     ghToken,
// 					Client:    &http.Client{Timeout: 10 * time.Second},
// 				}
// 				if err := gh.CreateOrUpdateIssue(deduplicatedFindings); err != nil {
// 					t.Errorf("Failed to create/update GitHub issue: %v", err)
// 				}
// 			} else {
// 				t.Log("Could not determine repository info for GitHub issue creation.")
// 			}
// 		}
// 	}
//
// 	// FAIL if ANY missing items (required or optional) exist:
// 	if len(deduplicatedFindings) > 0 {
// 		t.Fatalf("Found %d missing properties/blocks in root or submodules. See logs above.", len(deduplicatedFindings))
// 	}
// }
//
// func deduplicateFindings(findings []ValidationFinding) []ValidationFinding {
// 	if len(findings) == 0 {
// 		return nil
// 	}
//
// 	// Create a map to detect duplicates
// 	seen := make(map[string]bool)
// 	var result []ValidationFinding
//
// 	for _, f := range findings {
// 		// Create a unique key for each finding
// 		key := fmt.Sprintf("%s|%s|%s|%v|%v|%s",
// 			f.ResourceType,
// 			f.Path,
// 			f.Name,
// 			f.IsBlock,
// 			f.IsDataSource,
// 			f.SubmoduleName,
// 		)
//
// 		// Only add to the result if we haven't seen this exact finding before
// 		if !seen[key] {
// 			seen[key] = true
// 			result = append(result, f)
// 		}
// 	}
//
// 	return result
// }
//
// func validateTerraformSchemaInDir(t *testing.T, dir, submoduleName string) ([]ValidationFinding, error) {
// 	if dir == "" {
// 		return nil, fmt.Errorf("empty directory provided")
// 	}
//
// 	mainTf := filepath.Join(dir, "main.tf")
// 	if _, err := os.Stat(mainTf); err != nil {
// 		if os.IsNotExist(err) {
// 			return nil, nil
// 		}
// 		return nil, fmt.Errorf("error checking main.tf: %w", err)
// 	}
//
// 	parser := &DefaultHCLParser{}
// 	tfFile := filepath.Join(dir, "terraform.tf")
// 	providers, err := parser.ParseProviderRequirements(tfFile)
// 	if err != nil {
// 		return nil, fmt.Errorf("failed to parse provider config in %s: %w", dir, err)
// 	}
//
// 	// cleanup
// 	defer func() {
// 		// Ignore errors on cleanup operations
// 		_ = os.RemoveAll(filepath.Join(dir, ".terraform"))
// 		_ = os.Remove(filepath.Join(dir, "terraform.tfstate"))
// 		_ = os.Remove(filepath.Join(dir, ".terraform.lock.hcl"))
// 	}()
//
// 	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
// 	defer cancel()
//
// 	initCmd := exec.CommandContext(ctx, "terraform", "init")
// 	initCmd.Dir = dir
// 	out, err := initCmd.CombinedOutput()
// 	if err != nil {
// 		return nil, fmt.Errorf("terraform init failed in %s: %v\nOutput: %s", dir, err, string(out))
// 	}
//
// 	schemaCmd := exec.CommandContext(ctx, "terraform", "providers", "schema", "-json")
// 	schemaCmd.Dir = dir
// 	out, err = schemaCmd.Output()
// 	if err != nil {
// 		var exitErr *exec.ExitError
// 		stderr := ""
// 		if errors.As(err, &exitErr) {
// 			stderr = string(exitErr.Stderr)
// 		}
// 		return nil, fmt.Errorf("failed to get schema in %s: %w\nStderr: %s", dir, err, stderr)
// 	}
//
// 	var tfSchema TerraformSchema
// 	if err := json.Unmarshal(out, &tfSchema); err != nil {
// 		return nil, fmt.Errorf("failed to unmarshal schema in %s: %w", dir, err)
// 	}
//
// 	resources, dataSources, err := parser.ParseMainFile(mainTf)
// 	if err != nil {
// 		return nil, fmt.Errorf("parseMainFile in %s: %w", dir, err)
// 	}
//
// 	// compare resources
// 	var findings []ValidationFinding
//
// 	// Process resources
// 	for _, r := range resources {
// 		// e.g. "azurerm_virtual_hub_routing_intent" => provider name "azurerm"
// 		parts := strings.SplitN(r.Type, "_", 2)
// 		if len(parts) < 2 {
// 			t.Logf("Invalid resource type format: %s", r.Type)
// 			continue
// 		}
//
// 		provName := parts[0]
// 		cfg, ok := providers[provName]
// 		if !ok {
// 			t.Logf("No provider config for resource type %s in %s", r.Type, dir)
// 			continue
// 		}
//
// 		pSchema, ok := tfSchema.ProviderSchemas[cfg.Source]
// 		if !ok || pSchema == nil {
// 			t.Logf("No provider schema found for source %s in %s", cfg.Source, dir)
// 			continue
// 		}
//
// 		resSchema, ok := pSchema.ResourceSchemas[r.Type]
// 		if !ok || resSchema == nil || resSchema.Block == nil {
// 			t.Logf("No resource schema found for %s in provider %s (dir=%s)", r.Type, cfg.Source, dir)
// 			continue
// 		}
//
// 		var local []ValidationFinding
// 		// Pass resource-level ignoreChanges to initial validation
// 		r.data.Validate(t, r.Type, "root", resSchema.Block, r.data.ignoreChanges, &local)
//
// 		// Filter out any findings for attributes that are in ignore_changes (case-insensitively)
// 		for i := range local {
// 			// Check if the finding should be excluded due to ignore_changes
// 			shouldExclude := false
// 			for _, ignored := range r.data.ignoreChanges {
// 				if strings.EqualFold(ignored, local[i].Name) {
// 					shouldExclude = true
// 					break
// 				}
// 			}
//
// 			if !shouldExclude {
// 				local[i].SubmoduleName = submoduleName
// 				findings = append(findings, local[i])
// 			}
// 		}
// 	}
//
// 	// Process data sources
// 	for _, ds := range dataSources {
// 		parts := strings.SplitN(ds.Type, "_", 2)
// 		if len(parts) < 2 {
// 			t.Logf("Invalid data source type format: %s", ds.Type)
// 			continue
// 		}
//
// 		provName := parts[0]
// 		cfg, ok := providers[provName]
// 		if !ok {
// 			t.Logf("No provider config for data source type %s in %s", ds.Type, dir)
// 			continue
// 		}
//
// 		pSchema, ok := tfSchema.ProviderSchemas[cfg.Source]
// 		if !ok || pSchema == nil {
// 			t.Logf("No provider schema found for source %s in %s", cfg.Source, dir)
// 			continue
// 		}
//
// 		dsSchema, ok := pSchema.DataSourceSchemas[ds.Type]
// 		if !ok || dsSchema == nil || dsSchema.Block == nil {
// 			t.Logf("No data source schema found for %s in provider %s (dir=%s)", ds.Type, cfg.Source, dir)
// 			continue
// 		}
//
// 		var local []ValidationFinding
// 		// Pass data source-level ignoreChanges to initial validation
// 		ds.data.Validate(t, ds.Type, "root", dsSchema.Block, ds.data.ignoreChanges, &local)
//
// 		// Filter out any findings that should be excluded (case-insensitively)
// 		for i := range local {
// 			shouldExclude := false
// 			for _, ignored := range ds.data.ignoreChanges {
// 				if strings.EqualFold(ignored, local[i].Name) {
// 					shouldExclude = true
// 					break
// 				}
// 			}
//
// 			if !shouldExclude {
// 				local[i].SubmoduleName = submoduleName
// 				local[i].IsDataSource = true // Mark as data source
// 				findings = append(findings, local[i])
// 			}
// 		}
// 	}
//
// 	return findings, nil
// }
//
// // findSubmodules => subdirectories under modulesDir (one level) that contain main.tf
// func findSubmodules(modulesDir string) ([]struct {
// 	name string
// 	path string
// }, error) {
// 	var result []struct {
// 		name string
// 		path string
// 	}
//
// 	if modulesDir == "" {
// 		return result, nil
// 	}
//
// 	entries, err := os.ReadDir(modulesDir)
// 	if err != nil {
// 		if os.IsNotExist(err) {
// 			return result, nil
// 		}
// 		return result, fmt.Errorf("failed to read modules directory: %w", err)
// 	}
//
// 	for _, e := range entries {
// 		if e.IsDir() {
// 			subName := e.Name()
// 			subPath := filepath.Join(modulesDir, subName)
// 			mainTf := filepath.Join(subPath, "main.tf")
// 			if _, err := os.Stat(mainTf); err == nil {
// 				result = append(result, struct {
// 					name string
// 					path string
// 				}{subName, subPath})
// 			}
// 		}
// 	}
// 	return result, nil
// }
//
// func logMissingAttribute(t *testing.T, resType, name, path string, required bool) {
// 	status := boolToStr(required, "required", "optional")
// 	cpath := strings.ReplaceAll(path, "root.", "")
// 	t.Logf("%s missing %s property %q in %s", resType, status, name, cpath)
// }
//
// func logMissingBlock(t *testing.T, resType, name, path string, required bool) {
// 	status := boolToStr(required, "required", "optional")
// 	cpath := strings.ReplaceAll(path, "root.", "")
// 	t.Logf("%s missing %s block %q in %s", resType, status, name, cpath)
// }
//
// func boolToStr(cond bool, yes, no string) string {
// 	if cond {
// 		return yes
// 	}
// 	return no
// }
//
// func normalizeSource(source string) string {
// 	if source == "" {
// 		return ""
// 	}
//
// 	// e.g. "hashicorp/azurerm" => "registry.terraform.io/hashicorp/azurerm"
// 	if strings.Contains(source, "/") && !strings.Contains(source, "registry.terraform.io/") {
// 		return "registry.terraform.io/" + source
// 	}
// 	return source
// }
//
// // Merges dynamic blocks, ignoring etc
// func mergeBlocks(dest, src *ParsedBlock) {
// 	if dest == nil || src == nil {
// 		return
// 	}
//
// 	for k := range src.data.properties {
// 		dest.data.properties[k] = true
// 	}
//
// 	for k, v := range src.data.staticBlocks {
// 		if existing, ok := dest.data.staticBlocks[k]; ok {
// 			mergeBlocks(existing, v)
// 		} else {
// 			dest.data.staticBlocks[k] = v
// 		}
// 	}
//
// 	for k, v := range src.data.dynamicBlocks {
// 		if existing, ok := dest.data.dynamicBlocks[k]; ok {
// 			mergeBlocks(existing, v)
// 		} else {
// 			dest.data.dynamicBlocks[k] = v
// 		}
// 	}
//
// 	dest.data.ignoreChanges = append(dest.data.ignoreChanges, src.data.ignoreChanges...)
// }
//
// func extractIgnoreChanges(val cty.Value) []string {
// 	var changes []string
//
// 	if !val.IsKnown() || val.IsNull() {
// 		return changes
// 	}
//
// 	if val.Type().IsCollectionType() {
// 		for it := val.ElementIterator(); it.Next(); {
// 			_, v := it.Element()
// 			if v.Type() == cty.String {
// 				str := v.AsString()
// 				// If "all" is specified, return a special marker
// 				if str == "all" {
// 					return []string{"*all*"}
// 				}
// 				changes = append(changes, str)
// 			}
// 		}
// 	}
// 	return changes
// }
//
// func findContentBlock(body *hclsyntax.Body) *hclsyntax.Body {
// 	if body == nil {
// 		return nil
// 	}
//
// 	for _, b := range body.Blocks {
// 		if b != nil && b.Type == "content" && b.Body != nil {
// 			return b.Body
// 		}
// 	}
// 	return body
// }
//
// func ParseSyntaxBody(body *hclsyntax.Body) *ParsedBlock {
// 	if body == nil {
// 		return &ParsedBlock{data: NewBlockData()}
// 	}
//
// 	bd := NewBlockData()
// 	blk := &ParsedBlock{data: bd}
// 	bd.ParseAttributes(body)
// 	bd.ParseBlocks(body)
// 	return blk
// }

// package main
//
// import (
// 	"bytes"
// 	"context"
// 	"encoding/json"
// 	"fmt"
// 	"github.com/hashicorp/hcl/v2/hclparse"
// 	"github.com/hashicorp/hcl/v2/hclsyntax"
// 	"github.com/zclconf/go-cty/cty"
// 	"golang.org/x/exp/slices"
// 	"net/http"
// 	"os"
// 	"os/exec"
// 	"path/filepath"
// 	"strings"
// 	"testing"
// 	"time"
// )
//
// type BlockProcessor interface {
// 	ParseAttributes(body *hclsyntax.Body)
// 	ParseBlocks(body *hclsyntax.Body)
// 	Validate(t *testing.T, resourceType, path string, schema *SchemaBlock, parentIgnore []string, findings *[]ValidationFinding)
// }
//
// type HCLParser interface {
// 	ParseProviderRequirements(filename string) (map[string]ProviderConfig, error)
// 	ParseMainFile(filename string) ([]ParsedResource, []ParsedDataSource, error)
// }
//
// type IssueManager interface {
// 	CreateOrUpdateIssue(findings []ValidationFinding) error
// }
//
// type RepositoryInfoProvider interface {
// 	GetRepoInfo() (owner, name string)
// }
//
// // schema definitions
// type TerraformSchema struct {
// 	ProviderSchemas map[string]*ProviderSchema `json:"provider_schemas"`
// }
//
// type ProviderSchema struct {
// 	ResourceSchemas   map[string]*ResourceSchema `json:"resource_schemas"`
// 	DataSourceSchemas map[string]*ResourceSchema `json:"data_source_schemas"`
// }
//
// type ResourceSchema struct {
// 	Block *SchemaBlock `json:"block"`
// }
//
// type SchemaBlock struct {
// 	Attributes map[string]*SchemaAttribute `json:"attributes"`
// 	BlockTypes map[string]*SchemaBlockType `json:"block_types"`
// }
//
// type SchemaAttribute struct {
// 	Required bool `json:"required"`
// 	Optional bool `json:"optional"`
// 	Computed bool `json:"computed"`
// }
//
// type SchemaBlockType struct {
// 	Nesting  string       `json:"nesting"`
// 	MinItems int          `json:"min_items"`
// 	MaxItems int          `json:"max_items"`
// 	Block    *SchemaBlock `json:"block"`
// }
//
// // ValidationFinding logs any missing attribute/block.
// type ValidationFinding struct {
// 	ResourceType  string
// 	Path          string // e.g., "root" or "root.some_nested_block"
// 	Name          string
// 	Required      bool
// 	IsBlock       bool
// 	IsDataSource  bool   // If true, this is a data source, not a resource
// 	SubmoduleName string // empty => root, else submodule name
// }
//
// type ProviderConfig struct {
// 	Source  string
// 	Version string
// }
//
// type ParsedResource struct {
// 	Type string
// 	Name string
// 	data BlockData
// }
//
// type ParsedDataSource struct {
// 	Type string
// 	Name string
// 	data BlockData
// }
//
// type BlockData struct {
// 	properties    map[string]bool
// 	staticBlocks  map[string]*ParsedBlock
// 	dynamicBlocks map[string]*ParsedBlock
// 	ignoreChanges []string
// }
//
// type ParsedBlock struct {
// 	data BlockData
// }
//
// func NewBlockData() BlockData {
// 	return BlockData{
// 		properties:    make(map[string]bool),
// 		staticBlocks:  make(map[string]*ParsedBlock),
// 		dynamicBlocks: make(map[string]*ParsedBlock),
// 		ignoreChanges: []string{},
// 	}
// }
//
// // Extract ignore_changes directly from AST
// func extractLifecycleIgnoreChangesFromAST(body *hclsyntax.Body) []string {
// 	var ignoreChanges []string
//
// 	for _, block := range body.Blocks {
// 		if block.Type == "lifecycle" {
// 			for name, attr := range block.Body.Attributes {
// 				if name == "ignore_changes" {
// 					if listExpr, ok := attr.Expr.(*hclsyntax.TupleConsExpr); ok {
// 						for _, expr := range listExpr.Exprs {
// 							if traversalExpr, ok := expr.(*hclsyntax.ScopeTraversalExpr); ok {
// 								if len(traversalExpr.Traversal) > 0 {
// 									ignoreChanges = append(ignoreChanges, traversalExpr.Traversal.RootName())
// 								}
// 							} else if templateExpr, ok := expr.(*hclsyntax.TemplateExpr); ok {
// 								if len(templateExpr.Parts) == 1 {
// 									if literalPart, ok := templateExpr.Parts[0].(*hclsyntax.LiteralValueExpr); ok {
// 										if literalPart.Val.Type() == cty.String {
// 											ignoreChanges = append(ignoreChanges, literalPart.Val.AsString())
// 										}
// 									}
// 								}
// 							} else if literalExpr, ok := expr.(*hclsyntax.LiteralValueExpr); ok {
// 								if literalExpr.Val.Type() == cty.String {
// 									ignoreChanges = append(ignoreChanges, literalExpr.Val.AsString())
// 								}
// 							}
// 						}
// 					}
// 				}
// 			}
// 		}
// 	}
//
// 	return ignoreChanges
// }
//
// func (bd *BlockData) ParseAttributes(body *hclsyntax.Body) {
// 	for name := range body.Attributes {
// 		bd.properties[name] = true
// 	}
// }
//
// func (bd *BlockData) ParseBlocks(body *hclsyntax.Body) {
// 	// First, directly extract any lifecycle ignore_changes from AST
// 	directIgnoreChanges := extractLifecycleIgnoreChangesFromAST(body)
// 	if len(directIgnoreChanges) > 0 {
// 		bd.ignoreChanges = append(bd.ignoreChanges, directIgnoreChanges...)
// 	}
//
// 	for _, block := range body.Blocks {
// 		switch block.Type {
// 		case "lifecycle":
// 			bd.parseLifecycle(block.Body)
// 		case "dynamic":
// 			if len(block.Labels) == 1 {
// 				bd.parseDynamicBlock(block.Body, block.Labels[0])
// 			}
// 		default:
// 			parsed := ParseSyntaxBody(block.Body)
// 			bd.staticBlocks[block.Type] = parsed
// 		}
// 	}
// }
//
// func (bd *BlockData) Validate(
// 	t *testing.T,
// 	resourceType, path string,
// 	schema *SchemaBlock,
// 	parentIgnore []string,
// 	findings *[]ValidationFinding,
// ) {
// 	if schema == nil {
// 		return
// 	}
// 	// Combine parent ignore changes with this block's ignore changes
// 	ignore := append([]string{}, parentIgnore...)
// 	ignore = append(ignore, bd.ignoreChanges...)
//
// 	bd.validateAttributes(t, resourceType, path, schema, ignore, findings)
// 	bd.validateBlocks(t, resourceType, path, schema, ignore, findings)
// }
//
// // parseLifecycle picks up ignore_changes in lifecycle { }
// func (bd *BlockData) parseLifecycle(body *hclsyntax.Body) {
// 	for name, attr := range body.Attributes {
// 		if name == "ignore_changes" {
// 			val, diags := attr.Expr.Value(nil)
// 			// Only process if we could get a value without errors
// 			if diags == nil || !diags.HasErrors() {
// 				extracted := extractIgnoreChanges(val)
// 				bd.ignoreChanges = append(bd.ignoreChanges, extracted...)
// 			}
// 		}
// 	}
// }
//
// func (bd *BlockData) parseDynamicBlock(body *hclsyntax.Body, name string) {
// 	contentBlock := findContentBlock(body)
// 	parsed := ParseSyntaxBody(contentBlock)
// 	if existing := bd.dynamicBlocks[name]; existing != nil {
// 		mergeBlocks(existing, parsed)
// 	} else {
// 		bd.dynamicBlocks[name] = parsed
// 	}
// }
//
// func (bd *BlockData) validateAttributes(
// 	t *testing.T,
// 	resType, path string,
// 	schema *SchemaBlock,
// 	ignore []string,
// 	findings *[]ValidationFinding,
// ) {
// 	for name, attr := range schema.Attributes {
// 		// Skip 'id' property as it's not useful to show
// 		if name == "id" {
// 			continue
// 		}
//
// 		// Skip purely computed attributes (those that are computed but not optional)
// 		// These are always exported, never set by the user
// 		if attr.Computed && !attr.Optional && !attr.Required {
// 			continue
// 		}
//
// 		// Skip attributes in the ignore list (case insensitive)
// 		if isIgnored(ignore, name) {
// 			continue
// 		}
//
// 		if !bd.properties[name] {
// 			*findings = append(*findings, ValidationFinding{
// 				ResourceType: resType,
// 				Path:         path,
// 				Name:         name,
// 				Required:     attr.Required,
// 				IsBlock:      false,
// 			})
// 			// No direct logging here to avoid duplication
// 		}
// 	}
// }
//
// func (bd *BlockData) validateBlocks(
// 	t *testing.T,
// 	resType, path string,
// 	schema *SchemaBlock,
// 	ignore []string,
// 	findings *[]ValidationFinding,
// ) {
// 	for name, blockType := range schema.BlockTypes {
// 		// skip timeouts or ignored blocks (case insensitive)
// 		if name == "timeouts" || isIgnored(ignore, name) {
// 			continue
// 		}
// 		static := bd.staticBlocks[name]
// 		dynamic := bd.dynamicBlocks[name]
// 		if static == nil && dynamic == nil {
// 			*findings = append(*findings, ValidationFinding{
// 				ResourceType: resType,
// 				Path:         path,
// 				Name:         name,
// 				Required:     blockType.MinItems > 0,
// 				IsBlock:      true,
// 			})
// 			// No direct logging here to avoid duplication
// 			continue
// 		}
// 		var target *ParsedBlock
// 		if static != nil {
// 			target = static
// 		} else {
// 			target = dynamic
// 		}
// 		newPath := fmt.Sprintf("%s.%s", path, name)
// 		target.data.Validate(t, resType, newPath, blockType.Block, ignore, findings)
// 	}
// }
//
// // Helper function to check if an item is ignored (case insensitive)
// func isIgnored(ignore []string, name string) bool {
// 	// Check for the special "all" marker
// 	if slices.Contains(ignore, "*all*") {
// 		return true
// 	}
//
// 	// Do a case-insensitive check
// 	for _, item := range ignore {
// 		if strings.EqualFold(item, name) {
// 			return true
// 		}
// 	}
// 	return false
// }
//
// // parser
// type DefaultHCLParser struct{}
//
// func (p *DefaultHCLParser) ParseProviderRequirements(filename string) (map[string]ProviderConfig, error) {
// 	parser := hclparse.NewParser()
// 	if _, err := os.Stat(filename); os.IsNotExist(err) {
// 		return map[string]ProviderConfig{}, nil
// 	}
// 	f, diags := parser.ParseHCLFile(filename)
// 	if diags.HasErrors() {
// 		return nil, fmt.Errorf("parse error in file %s: %v", filename, diags)
// 	}
// 	body, ok := f.Body.(*hclsyntax.Body)
// 	if !ok {
// 		return nil, fmt.Errorf("invalid body in file %s", filename)
// 	}
// 	providers := make(map[string]ProviderConfig)
// 	for _, blk := range body.Blocks {
// 		if blk.Type == "terraform" {
// 			for _, innerBlk := range blk.Body.Blocks {
// 				if innerBlk.Type == "required_providers" {
// 					attrs, _ := innerBlk.Body.JustAttributes()
// 					for name, attr := range attrs {
// 						val, _ := attr.Expr.Value(nil)
// 						if val.Type().IsObjectType() {
// 							pc := ProviderConfig{}
// 							if sourceVal := val.GetAttr("source"); !sourceVal.IsNull() {
// 								pc.Source = normalizeSource(sourceVal.AsString())
// 							}
// 							if versionVal := val.GetAttr("version"); !versionVal.IsNull() {
// 								pc.Version = versionVal.AsString()
// 							}
// 							providers[name] = pc
// 						}
// 					}
// 				}
// 			}
// 		}
// 	}
// 	return providers, nil
// }
//
// func (p *DefaultHCLParser) ParseMainFile(filename string) ([]ParsedResource, []ParsedDataSource, error) {
// 	parser := hclparse.NewParser()
// 	f, diags := parser.ParseHCLFile(filename)
// 	if diags.HasErrors() {
// 		return nil, nil, fmt.Errorf("parse error in file %s: %v", filename, diags)
// 	}
// 	body, ok := f.Body.(*hclsyntax.Body)
// 	if !ok {
// 		return nil, nil, fmt.Errorf("invalid body in file %s", filename)
// 	}
// 	var resources []ParsedResource
// 	var dataSources []ParsedDataSource
//
// 	for _, blk := range body.Blocks {
// 		// Parse resources
// 		if blk.Type == "resource" && len(blk.Labels) >= 2 {
// 			parsed := ParseSyntaxBody(blk.Body)
//
// 			// Direct AST extraction of ignore_changes as a backup
// 			ignoreChanges := extractLifecycleIgnoreChangesFromAST(blk.Body)
// 			if len(ignoreChanges) > 0 {
// 				parsed.data.ignoreChanges = append(parsed.data.ignoreChanges, ignoreChanges...)
// 			}
//
// 			res := ParsedResource{
// 				Type: blk.Labels[0],
// 				Name: blk.Labels[1],
// 				data: parsed.data,
// 			}
// 			resources = append(resources, res)
// 		}
//
// 		// Parse data sources
// 		if blk.Type == "data" && len(blk.Labels) >= 2 {
// 			parsed := ParseSyntaxBody(blk.Body)
//
// 			// Direct AST extraction for data sources too
// 			ignoreChanges := extractLifecycleIgnoreChangesFromAST(blk.Body)
// 			if len(ignoreChanges) > 0 {
// 				parsed.data.ignoreChanges = append(parsed.data.ignoreChanges, ignoreChanges...)
// 			}
//
// 			ds := ParsedDataSource{
// 				Type: blk.Labels[0],
// 				Name: blk.Labels[1],
// 				data: parsed.data,
// 			}
// 			dataSources = append(dataSources, ds)
// 		}
// 	}
// 	return resources, dataSources, nil
// }
//
// // github issues
// type GitHubIssueService struct {
// 	RepoOwner string
// 	RepoName  string
// 	token     string
// 	Client    *http.Client
// }
//
// func (g *GitHubIssueService) CreateOrUpdateIssue(findings []ValidationFinding) error {
// 	if len(findings) == 0 {
// 		return nil
// 	}
//
// 	const header = "### \n\n"
// 	dedup := make(map[string]ValidationFinding)
//
// 	// Deduplicate exact lines in the GitHub issue (just to avoid repeating the same line).
// 	for _, f := range findings {
// 		key := fmt.Sprintf("%s|%s|%s|%v|%v|%s",
// 			f.ResourceType,
// 			strings.ReplaceAll(f.Path, "root.", ""),
// 			f.Name,
// 			f.IsBlock,
// 			f.IsDataSource,
// 			f.SubmoduleName,
// 		)
// 		dedup[key] = f
// 	}
//
// 	var newBody bytes.Buffer
// 	fmt.Fprint(&newBody, header)
// 	for _, f := range dedup {
// 		cleanPath := strings.ReplaceAll(f.Path, "root.", "")
// 		status := boolToStr(f.Required, "required", "optional")
// 		itemType := boolToStr(f.IsBlock, "block", "property")
// 		entityType := "resource"
// 		if f.IsDataSource {
// 			entityType = "data source"
// 		}
//
// 		if f.SubmoduleName == "" {
// 			fmt.Fprintf(&newBody, "`%s`: missing %s %s `%s` in `%s` (%s)\n\n",
// 				f.ResourceType, status, itemType, f.Name, cleanPath, entityType,
// 			)
// 		} else {
// 			fmt.Fprintf(&newBody, "`%s`: missing %s %s `%s` in `%s` in submodule `%s` (%s)\n\n",
// 				f.ResourceType, status, itemType, f.Name, cleanPath, f.SubmoduleName, entityType,
// 			)
// 		}
// 	}
//
// 	title := "Generated schema validation"
// 	issueNum, existingBody, err := g.findExistingIssue(title)
// 	if err != nil {
// 		return err
// 	}
// 	finalBody := newBody.String()
// 	if issueNum > 0 {
// 		parts := strings.SplitN(existingBody, header, 2)
// 		if len(parts) > 0 {
// 			finalBody = strings.TrimSpace(parts[0]) + "\n\n" + newBody.String()
// 		}
// 		return g.updateIssue(issueNum, finalBody)
// 	}
// 	return g.createIssue(title, finalBody)
// }
//
// func (g *GitHubIssueService) findExistingIssue(title string) (int, string, error) {
// 	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues?state=open", g.RepoOwner, g.RepoName)
// 	req, _ := http.NewRequest("GET", url, nil)
// 	req.Header.Set("Authorization", "token "+g.token)
// 	req.Header.Set("Accept", "application/vnd.github.v3+json")
//
// 	resp, err := g.Client.Do(req)
// 	if err != nil {
// 		return 0, "", err
// 	}
// 	defer resp.Body.Close()
// 	if resp.StatusCode != http.StatusOK {
// 		return 0, "", fmt.Errorf("GitHub API error: %s", resp.Status)
// 	}
//
// 	var issues []struct {
// 		Number int    `json:"number"`
// 		Title  string `json:"title"`
// 		Body   string `json:"body"`
// 	}
// 	if err := json.NewDecoder(resp.Body).Decode(&issues); err != nil {
// 		return 0, "", err
// 	}
// 	for _, issue := range issues {
// 		if issue.Title == title {
// 			return issue.Number, issue.Body, nil
// 		}
// 	}
// 	return 0, "", nil
// }
//
// func (g *GitHubIssueService) updateIssue(issueNumber int, body string) error {
// 	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d", g.RepoOwner, g.RepoName, issueNumber)
// 	payload := struct {
// 		Body string `json:"body"`
// 	}{Body: body}
// 	data, _ := json.Marshal(payload)
//
// 	req, _ := http.NewRequest("PATCH", url, bytes.NewReader(data))
// 	req.Header.Set("Authorization", "token "+g.token)
// 	req.Header.Set("Content-Type", "application/json")
//
// 	resp, err := g.Client.Do(req)
// 	if err != nil {
// 		return err
// 	}
// 	defer resp.Body.Close()
// 	return nil
// }
//
// func (g *GitHubIssueService) createIssue(title, body string) error {
// 	payload := struct {
// 		Title string `json:"title"`
// 		Body  string `json:"body"`
// 	}{
// 		Title: title,
// 		Body:  body,
// 	}
// 	data, _ := json.Marshal(payload)
//
// 	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues", g.RepoOwner, g.RepoName)
// 	req, _ := http.NewRequest("POST", url, bytes.NewReader(data))
// 	req.Header.Set("Authorization", "token "+g.token)
// 	req.Header.Set("Content-Type", "application/json")
//
// 	resp, err := g.Client.Do(req)
// 	if err != nil {
// 		return err
// 	}
// 	defer resp.Body.Close()
// 	return nil
// }
//
// // repository info
// type GitRepoInfo struct {
// 	terraformRoot string
// }
//
// func (g *GitRepoInfo) GetRepoInfo() (owner, repo string) {
// 	owner = os.Getenv("GITHUB_REPOSITORY_OWNER")
// 	repo = os.Getenv("GITHUB_REPOSITORY_NAME")
// 	if owner != "" && repo != "" {
// 		return owner, repo
// 	}
//
// 	if ghRepo := os.Getenv("GITHUB_REPOSITORY"); ghRepo != "" {
// 		parts := strings.SplitN(ghRepo, "/", 2)
// 		if len(parts) == 2 {
// 			owner, repo = parts[0], parts[1]
// 			return owner, repo
// 		}
// 	}
// 	return "", ""
// }
//
// func TestValidateTerraformSchema(t *testing.T) {
// 	// root directory from env or "."
// 	terraformRoot := os.Getenv("TERRAFORM_ROOT")
// 	if terraformRoot == "" {
// 		terraformRoot = "."
// 	}
//
// 	// Validate root
// 	rootFindings, err := validateTerraformSchemaInDir(t, terraformRoot, "")
// 	if err != nil {
// 		t.Fatalf("Failed to validate root at %s: %v", terraformRoot, err)
// 	}
// 	var allFindings []ValidationFinding
// 	allFindings = append(allFindings, rootFindings...)
//
// 	// Validate submodules in modules/<n>/ (one level)
// 	modulesDir := filepath.Join(terraformRoot, "modules")
// 	subs, err := findSubmodules(modulesDir)
// 	if err != nil {
// 		t.Fatalf("Failed to find submodules in %s: %v", modulesDir, err)
// 	}
// 	for _, sm := range subs {
// 		f, sErr := validateTerraformSchemaInDir(t, sm.path, sm.name)
// 		if sErr != nil {
// 			t.Errorf("Failed to validate submodule %s: %v", sm.name, sErr)
// 			continue
// 		}
// 		allFindings = append(allFindings, f...)
// 	}
//
// 	// Deduplicate findings before logging
// 	deduplicatedFindings := deduplicateFindings(allFindings)
//
// 	// Log all missing
// 	for _, f := range deduplicatedFindings {
// 		place := "root"
// 		if f.SubmoduleName != "" {
// 			place = "root in submodule " + f.SubmoduleName
// 		}
// 		requiredOptional := boolToStr(f.Required, "required", "optional")
// 		blockOrProp := boolToStr(f.IsBlock, "block", "property")
// 		entityType := "resource"
// 		if f.IsDataSource {
// 			entityType = "data source"
// 		}
// 		t.Logf("%s missing %s %s %q in %s (%s)", f.ResourceType, requiredOptional, blockOrProp, f.Name, place, entityType)
// 	}
//
// 	// If GITHUB_TOKEN is set, create/update single GH issue
// 	if ghToken := os.Getenv("GITHUB_TOKEN"); ghToken != "" {
// 		if len(deduplicatedFindings) > 0 {
// 			gi := &GitRepoInfo{terraformRoot: terraformRoot}
// 			owner, repoName := gi.GetRepoInfo()
// 			if owner != "" && repoName != "" {
// 				gh := &GitHubIssueService{
// 					RepoOwner: owner,
// 					RepoName:  repoName,
// 					token:     ghToken,
// 					Client:    &http.Client{Timeout: 10 * time.Second},
// 				}
// 				if err := gh.CreateOrUpdateIssue(deduplicatedFindings); err != nil {
// 					t.Errorf("Failed to create/update GitHub issue: %v", err)
// 				}
// 			} else {
// 				t.Log("Could not determine repository info for GitHub issue creation.")
// 			}
// 		}
// 	}
//
// 	// FAIL if ANY missing items (required or optional) exist:
// 	if len(deduplicatedFindings) > 0 {
// 		t.Fatalf("Found %d missing properties/blocks in root or submodules. See logs above.", len(deduplicatedFindings))
// 	}
// }
//
// func deduplicateFindings(findings []ValidationFinding) []ValidationFinding {
// 	// Create a map to detect duplicates
// 	seen := make(map[string]bool)
// 	var result []ValidationFinding
//
// 	for _, f := range findings {
// 		// Create a unique key for each finding
// 		key := fmt.Sprintf("%s|%s|%s|%v|%v|%s",
// 			f.ResourceType,
// 			f.Path,
// 			f.Name,
// 			f.IsBlock,
// 			f.IsDataSource,
// 			f.SubmoduleName,
// 		)
//
// 		// Only add to the result if we haven't seen this exact finding before
// 		if !seen[key] {
// 			seen[key] = true
// 			result = append(result, f)
// 		}
// 	}
//
// 	return result
// }
//
// func validateTerraformSchemaInDir(t *testing.T, dir, submoduleName string) ([]ValidationFinding, error) {
// 	mainTf := filepath.Join(dir, "main.tf")
// 	if _, err := os.Stat(mainTf); os.IsNotExist(err) {
// 		return nil, nil
// 	}
//
// 	parser := &DefaultHCLParser{}
// 	tfFile := filepath.Join(dir, "terraform.tf")
// 	providers, err := parser.ParseProviderRequirements(tfFile)
// 	if err != nil {
// 		return nil, fmt.Errorf("failed to parse provider config in %s: %w", dir, err)
// 	}
//
// 	// cleanup
// 	defer func() {
// 		os.RemoveAll(filepath.Join(dir, ".terraform"))
// 		os.Remove(filepath.Join(dir, "terraform.tfstate"))
// 		os.Remove(filepath.Join(dir, ".terraform.lock.hcl"))
// 	}()
//
// 	initCmd := exec.CommandContext(context.Background(), "terraform", "init")
// 	initCmd.Dir = dir
// 	if out, err := initCmd.CombinedOutput(); err != nil {
// 		return nil, fmt.Errorf("terraform init failed in %s: %v\nOutput: %s", dir, err, string(out))
// 	}
//
// 	schemaCmd := exec.CommandContext(context.Background(), "terraform", "providers", "schema", "-json")
// 	schemaCmd.Dir = dir
// 	out, err := schemaCmd.Output()
// 	if err != nil {
// 		return nil, fmt.Errorf("failed to get schema in %s: %w", dir, err)
// 	}
// 	var tfSchema TerraformSchema
// 	if err := json.Unmarshal(out, &tfSchema); err != nil {
// 		return nil, fmt.Errorf("failed to unmarshal schema in %s: %w", dir, err)
// 	}
//
// 	resources, dataSources, err := parser.ParseMainFile(mainTf)
// 	if err != nil {
// 		return nil, fmt.Errorf("parseMainFile in %s: %w", dir, err)
// 	}
//
// 	// compare resources
// 	var findings []ValidationFinding
//
// 	// Process resources
// 	for _, r := range resources {
// 		// e.g. "azurerm_virtual_hub_routing_intent" => provider name "azurerm"
// 		provName := strings.SplitN(r.Type, "_", 2)[0]
// 		cfg, ok := providers[provName]
// 		if !ok {
// 			t.Logf("No provider config for resource type %s in %s", r.Type, dir)
// 			continue
// 		}
// 		pSchema := tfSchema.ProviderSchemas[cfg.Source]
// 		if pSchema == nil {
// 			t.Logf("No provider schema found for source %s in %s", cfg.Source, dir)
// 			continue
// 		}
// 		resSchema := pSchema.ResourceSchemas[r.Type]
// 		if resSchema == nil {
// 			t.Logf("No resource schema found for %s in provider %s (dir=%s)", r.Type, cfg.Source, dir)
// 			continue
// 		}
//
// 		var local []ValidationFinding
// 		// Pass resource-level ignoreChanges to initial validation
// 		r.data.Validate(t, r.Type, "root", resSchema.Block, r.data.ignoreChanges, &local)
//
// 		// Filter out any findings for attributes that are in ignore_changes (case-insensitively)
// 		for i := range local {
// 			// Check if the finding should be excluded due to ignore_changes
// 			shouldExclude := false
// 			for _, ignored := range r.data.ignoreChanges {
// 				if strings.EqualFold(ignored, local[i].Name) {
// 					shouldExclude = true
// 					break
// 				}
// 			}
//
// 			if !shouldExclude {
// 				local[i].SubmoduleName = submoduleName
// 				findings = append(findings, local[i])
// 			}
// 		}
// 	}
//
// 	// Process data sources
// 	for _, ds := range dataSources {
// 		provName := strings.SplitN(ds.Type, "_", 2)[0]
// 		cfg, ok := providers[provName]
// 		if !ok {
// 			t.Logf("No provider config for data source type %s in %s", ds.Type, dir)
// 			continue
// 		}
// 		pSchema := tfSchema.ProviderSchemas[cfg.Source]
// 		if pSchema == nil {
// 			t.Logf("No provider schema found for source %s in %s", cfg.Source, dir)
// 			continue
// 		}
// 		dsSchema := pSchema.DataSourceSchemas[ds.Type]
// 		if dsSchema == nil {
// 			t.Logf("No data source schema found for %s in provider %s (dir=%s)", ds.Type, cfg.Source, dir)
// 			continue
// 		}
//
// 		var local []ValidationFinding
// 		// Pass data source-level ignoreChanges to initial validation
// 		ds.data.Validate(t, ds.Type, "root", dsSchema.Block, ds.data.ignoreChanges, &local)
//
// 		// Filter out any findings that should be excluded (case-insensitively)
// 		for i := range local {
// 			shouldExclude := false
// 			for _, ignored := range ds.data.ignoreChanges {
// 				if strings.EqualFold(ignored, local[i].Name) {
// 					shouldExclude = true
// 					break
// 				}
// 			}
//
// 			if !shouldExclude {
// 				local[i].SubmoduleName = submoduleName
// 				local[i].IsDataSource = true // Mark as data source
// 				findings = append(findings, local[i])
// 			}
// 		}
// 	}
//
// 	return findings, nil
// }
//
// // findSubmodules => subdirectories under modulesDir (one level) that contain main.tf
// func findSubmodules(modulesDir string) ([]struct {
// 	name string
// 	path string
// }, error) {
// 	var result []struct {
// 		name string
// 		path string
// 	}
// 	entries, err := os.ReadDir(modulesDir)
// 	if err != nil {
// 		return result, nil
// 	}
// 	for _, e := range entries {
// 		if e.IsDir() {
// 			subName := e.Name()
// 			subPath := filepath.Join(modulesDir, subName)
// 			mainTf := filepath.Join(subPath, "main.tf")
// 			if _, err := os.Stat(mainTf); err == nil {
// 				result = append(result, struct {
// 					name string
// 					path string
// 				}{subName, subPath})
// 			}
// 		}
// 	}
// 	return result, err
// }
//
// func logMissingAttribute(t *testing.T, resType, name, path string, required bool) {
// 	status := boolToStr(required, "required", "optional")
// 	cpath := strings.ReplaceAll(path, "root.", "")
// 	t.Logf("%s missing %s property %q in %s", resType, status, name, cpath)
// }
//
// func logMissingBlock(t *testing.T, resType, name, path string, required bool) {
// 	status := boolToStr(required, "required", "optional")
// 	cpath := strings.ReplaceAll(path, "root.", "")
// 	t.Logf("%s missing %s block %q in %s", resType, status, name, cpath)
// }
//
// func boolToStr(cond bool, yes, no string) string {
// 	if cond {
// 		return yes
// 	}
// 	return no
// }
//
// func normalizeSource(source string) string {
// 	// e.g. "hashicorp/azurerm" => "registry.terraform.io/hashicorp/azurerm"
// 	if strings.Contains(source, "/") && !strings.Contains(source, "registry.terraform.io/") {
// 		return "registry.terraform.io/" + source
// 	}
// 	return source
// }
//
// // Merges dynamic blocks, ignoring etc
// func mergeBlocks(dest, src *ParsedBlock) {
// 	for k := range src.data.properties {
// 		dest.data.properties[k] = true
// 	}
// 	for k, v := range src.data.staticBlocks {
// 		if existing, ok := dest.data.staticBlocks[k]; ok {
// 			mergeBlocks(existing, v)
// 		} else {
// 			dest.data.staticBlocks[k] = v
// 		}
// 	}
// 	for k, v := range src.data.dynamicBlocks {
// 		if existing, ok := dest.data.dynamicBlocks[k]; ok {
// 			mergeBlocks(existing, v)
// 		} else {
// 			dest.data.dynamicBlocks[k] = v
// 		}
// 	}
// 	dest.data.ignoreChanges = append(dest.data.ignoreChanges, src.data.ignoreChanges...)
// }
//
// func extractIgnoreChanges(val cty.Value) []string {
// 	var changes []string
// 	if val.Type().IsCollectionType() {
// 		for it := val.ElementIterator(); it.Next(); {
// 			_, v := it.Element()
// 			if v.Type() == cty.String {
// 				str := v.AsString()
// 				// If "all" is specified, return a special marker
// 				if str == "all" {
// 					return []string{"*all*"}
// 				}
// 				changes = append(changes, str)
// 			}
// 		}
// 	}
// 	return changes
// }
//
// func findContentBlock(body *hclsyntax.Body) *hclsyntax.Body {
// 	for _, b := range body.Blocks {
// 		if b.Type == "content" {
// 			return b.Body
// 		}
// 	}
// 	return body
// }
//
// func ParseSyntaxBody(body *hclsyntax.Body) *ParsedBlock {
// 	bd := NewBlockData()
// 	blk := &ParsedBlock{data: bd}
// 	bd.ParseAttributes(body)
// 	bd.ParseBlocks(body)
// 	return blk
// }
