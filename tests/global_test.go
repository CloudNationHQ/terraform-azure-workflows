package main

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	"mvdan.cc/xurls/v2"
)

type MarkdownValidation struct {
	ReadmePath string
	Data       string
}

type TerraformConfig struct {
	Resource []Resource `hcl:"resource,block"`
}

type Resource struct {
	Type       string   `hcl:"type,label"`
	Name       string   `hcl:"name,label"`
	Properties hcl.Body `hcl:",remain"`
}

func InitMarkdownTests(readmePath string) (*MarkdownValidation, error) {
	data, err := os.ReadFile(readmePath)
	if err != nil {
		return nil, err
	}

	return &MarkdownValidation{
		ReadmePath: readmePath,
		Data:       string(data),
	}, nil
}

func (ts *MarkdownValidation) ValidateURLs(t *testing.T) {
	rxStrict := xurls.Strict()
	urls := rxStrict.FindAllString(ts.Data, -1)

	var wg sync.WaitGroup
	for _, u := range urls {
		if strings.Contains(u, "registry.terraform.io/providers/") {
			continue
		}

		_, err := url.Parse(u)
		if err != nil {
			continue
		}

		wg.Add(1)
		go func(url string) {
			defer wg.Done()

			resp, err := http.Get(url)
			if err != nil {
				t.Errorf("Failed: URL: %s, Error: %v", url, err)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("Failed: URL: %s, Status code: %d", url, resp.StatusCode)
			}
		}(u)
	}
	wg.Wait()
}

func (ts *MarkdownValidation) ValidateReadmeHeaders(t *testing.T) {
	requiredHeaders := []string{
		"## Goals",
		"## Resources",
		"## Providers",
		"## Requirements",
		"## Inputs",
		"## Outputs",
		"## Features",
		"## Testing",
		"## Authors",
		"## License",
	}

	for _, requiredHeader := range requiredHeaders {
		if !strings.Contains(ts.Data, requiredHeader) {
			t.Errorf("Failed: README.md does not contain required header '%s'", requiredHeader)
		}
	}
}

func (ts *MarkdownValidation) ValidateReadmeNotEmpty(t *testing.T) {
	if len(ts.Data) == 0 {
		t.Errorf("Failed: README.md is empty.")
	}
}

func (ts *MarkdownValidation) ValidateTableHeaders(t *testing.T, header string, columns []string) {
	requiredHeaders := []string{"## " + header}
	for _, requiredHeader := range requiredHeaders {
		headerPattern := regexp.MustCompile("(?m)^" + regexp.QuoteMeta(requiredHeader) + "\\s*$")
		headerLoc := headerPattern.FindStringIndex(ts.Data)
		if headerLoc == nil {
			t.Errorf("Failed: README.md does not contain required header '%s'", requiredHeader)
			continue
		}

		tablePattern := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(requiredHeader) + `(\s*\|.*\|)+\s*`)
		tableLoc := tablePattern.FindStringIndex(ts.Data)
		if tableLoc == nil {
			t.Errorf("Failed: README.md does not contain a table immediately after the header '%s'", requiredHeader)
			continue
		}

		columnHeaders := strings.Join(columns, " \\| ")
		headerRowPattern := regexp.MustCompile(`(?m)\| ` + columnHeaders + ` \|`)
		headerRowLoc := headerRowPattern.FindStringIndex(ts.Data[tableLoc[0]:tableLoc[1]])
		if headerRowLoc == nil {
			t.Errorf(
				"Failed: README.md does not contain the correct column names in the table under header '%s'",
				requiredHeader,
			)
		}
	}
}

func ExtractReadmeResources(data string) ([]string, error) {
	var resources []string
	lines := strings.Split(data, "\n")
	inResourcesTable := false

	for _, line := range lines {
		if strings.TrimSpace(line) == "## Resources" {
			inResourcesTable = true
			continue
		}

		if inResourcesTable && strings.HasPrefix(line, "## ") {
			break
		}

		if inResourcesTable {
			if strings.Contains(line, "| [") {
				parts := strings.Split(line, "|")
				if len(parts) > 2 {
					resourceLink := strings.TrimSpace(parts[1])
					start := strings.Index(resourceLink, "[")
					end := strings.Index(resourceLink, "](")
					if start != -1 && end != -1 && start < end {
						resourceName := resourceLink[start+1 : end]
						resources = append(resources, resourceName)
					}
				}
			}
		}
	}
	return resources, nil
}

func ExtractTerraformResources() ([]string, error) {
	tffPath := filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "main.tf")
	parser := hclparse.NewParser()
	file, diags := parser.ParseHCLFile(tffPath)
	if diags.HasErrors() {
		return nil, errors.New(diags.Error())
	}

	var config TerraformConfig
	diags = gohcl.DecodeBody(file.Body, nil, &config)
	if diags.HasErrors() {
		return nil, errors.New(diags.Error())
	}

	var resources []string
	for _, resource := range config.Resource {
		resources = append(resources, resource.Type)
	}
	return resources, nil
}

func (ts *MarkdownValidation) ValidateTerraformDefinitions(t *testing.T) {
	tfResources, err := ExtractTerraformResources()
	if err != nil {
		t.Errorf("Failed to extract terraform resources: %v", err)
	}

	readmeResources, err := ExtractReadmeResources(ts.Data)
	if err != nil {
		t.Errorf("Failed to extract resources from markdown: %v", err)
	}

	for _, resource := range tfResources {
		if !contains(readmeResources, resource) {
			t.Errorf("Resource %s not found in markdown", resource)
		}
	}

	for _, resource := range readmeResources {
		if !contains(tfResources, resource) {
			t.Errorf("Resource %s not found in code", resource)
		}
	}
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func TestMarkdown(t *testing.T) {
	readmePath := os.Getenv("README_PATH")
	suite, err := InitMarkdownTests(readmePath)
	if err != nil {
		t.Fatalf("Failed to create test suite: %v", err)
	}

	t.Run("URLs", suite.ValidateURLs)
	t.Run("Headers", suite.ValidateReadmeHeaders)
	t.Run("NotEmpty", suite.ValidateReadmeNotEmpty)
	t.Run("ResourceTableHeaders", func(t *testing.T) {
		suite.ValidateTableHeaders(t, "Resources", []string{"Name", "Type"})
	})
	t.Run("TerraformDefinitions", suite.ValidateTerraformDefinitions)
}
