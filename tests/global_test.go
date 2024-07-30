package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"mvdan.cc/xurls/v2"
)

type Validator interface {
	Validate() []error
}

type SectionValidator interface {
	ValidateSection(data string) []error
}

type FileValidator interface {
	ValidateFile(filePath string) []error
}

type URLValidator interface {
	ValidateURLs(data string) []error
}

type TerraformValidator interface {
	ValidateTerraformDefinitions(data string) []error
}

type MarkdownValidator struct {
	readmePath   string
	data         string
	sections     []SectionValidator
	files        []FileValidator
	urlValidator URLValidator
	tfValidator  TerraformValidator
}

type Section struct {
	Header  string
	Columns []string
}

type RequiredFile struct {
	Name string
}

type StandardURLValidator struct{}

type TerraformDefinitionValidator struct{}

type TerraformConfig struct {
	Resource []Resource `hcl:"resource,block"`
	Data     []Data     `hcl:"data,block"`
}

type Resource struct {
	Type       string   `hcl:"type,label"`
	Name       string   `hcl:"name,label"`
	Properties hcl.Body `hcl:",remain"`
}

type Data struct {
	Type       string   `hcl:"type,label"`
	Name       string   `hcl:"name,label"`
	Properties hcl.Body `hcl:",remain"`
}

func NewMarkdownValidator(readmePath string) (*MarkdownValidator, error) {
	// Use environment variable set in github workflow
	if envPath := os.Getenv("README_PATH"); envPath != "" {
		readmePath = envPath
	}

	absReadmePath, err := filepath.Abs(readmePath)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %v", err)
	}

	data, err := os.ReadFile(absReadmePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %v", err)
	}

	sections := []SectionValidator{
		Section{Header: "Goals"},
		Section{Header: "Resources", Columns: []string{"Name", "Type"}},
		Section{Header: "Providers", Columns: []string{"Name", "Version"}},
		Section{Header: "Requirements", Columns: []string{"Name", "Version"}},
		Section{Header: "Inputs", Columns: []string{"Name", "Description", "Type", "Required"}},
		Section{Header: "Outputs", Columns: []string{"Name", "Description"}},
		Section{Header: "Features"},
		Section{Header: "Testing"},
		Section{Header: "Authors"},
		Section{Header: "License"},
	}

	rootDir := filepath.Dir(absReadmePath)

	files := []FileValidator{
		RequiredFile{Name: absReadmePath},
		RequiredFile{Name: filepath.Join(rootDir, "CONTRIBUTE.md")},
		RequiredFile{Name: filepath.Join(rootDir, "LICENSE")},
	}

	return &MarkdownValidator{
		readmePath:   absReadmePath,
		data:         string(data),
		sections:     sections,
		files:        files,
		urlValidator: StandardURLValidator{},
		tfValidator:  TerraformDefinitionValidator{},
	}, nil
}

func (mv *MarkdownValidator) Validate() []error {
	var allErrors []error

	allErrors = append(allErrors, mv.ValidateSections()...)
	allErrors = append(allErrors, mv.ValidateFiles()...)
	allErrors = append(allErrors, mv.ValidateURLs()...)
	allErrors = append(allErrors, mv.ValidateTerraformDefinitions()...)

	return allErrors
}

func (mv *MarkdownValidator) ValidateSections() []error {
	var allErrors []error
	for _, section := range mv.sections {
		allErrors = append(allErrors, section.ValidateSection(mv.data)...)
	}
	return allErrors
}

func (s Section) ValidateSection(data string) []error {
	var errors []error
	tableHeaderRegex := `^\s*\|(.+?)\|\s*(\r?\n)`

	flexibleHeaderPattern := regexp.MustCompile(`(?mi)^\s*##\s+` + strings.Replace(regexp.QuoteMeta(s.Header), `\s+`, `\s+`, -1) + `s?\s*$`)
	headerLoc := flexibleHeaderPattern.FindStringIndex(data)

	if headerLoc == nil {
		errors = append(errors, formatError("incorrect header: expected '## %s', found 'not present'", s.Header))
	} else {
		actualHeader := strings.TrimSpace(data[headerLoc[0]:headerLoc[1]])
		if actualHeader != "## "+s.Header {
			errors = append(errors, formatError("incorrect header: expected '## %s', found '%s'", s.Header, actualHeader))
		}

		if len(s.Columns) > 0 {
			startIdx := headerLoc[1]
			dataSlice := data[startIdx:]

			tableHeaderPattern := regexp.MustCompile(tableHeaderRegex)
			tableHeaderMatch := tableHeaderPattern.FindStringSubmatch(dataSlice)
			if tableHeaderMatch == nil {
				errors = append(errors, formatError("missing table after header: %s", actualHeader))
			} else {
				actualHeaders := parseHeaders(tableHeaderMatch[1])
				if !equalSlices(actualHeaders, s.Columns) {
					errors = append(errors, compareColumns(s.Header, s.Columns, actualHeaders))
				}
			}
		}
	}

	return errors
}

func (mv *MarkdownValidator) ValidateFiles() []error {
	var allErrors []error
	for _, file := range mv.files {
		allErrors = append(allErrors, file.ValidateFile(file.(RequiredFile).Name)...)
	}
	return allErrors
}

func (rf RequiredFile) ValidateFile(filePath string) []error {
	var errors []error
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			errors = append(errors, formatError("file does not exist:\n  %s", filePath))
		} else {
			errors = append(errors, formatError("error accessing file:\n  %s\n  %v", filePath, err))
		}
		return errors
	}

	if fileInfo.Size() == 0 {
		errors = append(errors, formatError("file is empty:\n  %s", filePath))
	}

	return errors
}

func (mv *MarkdownValidator) ValidateURLs() []error {
	return mv.urlValidator.ValidateURLs(mv.data)
}

func (suv StandardURLValidator) ValidateURLs(data string) []error {
	rxStrict := xurls.Strict()
	urls := rxStrict.FindAllString(data, -1)

	var wg sync.WaitGroup
	errChan := make(chan error, len(urls))

	for _, u := range urls {
		if strings.Contains(u, "registry.terraform.io/providers/") {
			continue
		}

		wg.Add(1)
		go func(url string) {
			defer wg.Done()
			if err := suv.validateSingleURL(url); err != nil {
				errChan <- err
			}
		}(u)
	}

	wg.Wait()
	close(errChan)

	var errors []error
	for err := range errChan {
		errors = append(errors, err)
	}

	return errors
}

func (suv StandardURLValidator) validateSingleURL(url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return formatError("error accessing URL:\n  %s\n  %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return formatError("URL returned non-OK status:\n  %s\n  Status: %d", url, resp.StatusCode)
	}

	return nil
}

func (mv *MarkdownValidator) ValidateTerraformDefinitions() []error {
	return mv.tfValidator.ValidateTerraformDefinitions(mv.data)
}

func (tdv TerraformDefinitionValidator) ValidateTerraformDefinitions(data string) []error {
	tfResources, err := extractTerraformResources()
	if err != nil {
		return []error{err}
	}

	readmeResources, err := extractReadmeResources(data)
	if err != nil {
		return []error{err}
	}

	var errors []error

	missingInMarkdown := findMissingItems(tfResources, readmeResources)
	if len(missingInMarkdown) > 0 {
		errors = append(errors, formatError("missing in markdown:\n  %s", strings.Join(missingInMarkdown, "\n  ")))
	}

	missingInCode := findMissingItems(readmeResources, tfResources)
	if len(missingInCode) > 0 {
		errors = append(errors, formatError("missing in code:\n  %s", strings.Join(missingInCode, "\n  ")))
	}

	return errors
}

func extractReadmeResources(data string) ([]string, error) {
	var resources []string
	resourcesPattern := regexp.MustCompile(`(?s)## Resources.*?\n(.*?)\n##`)
	resourcesSection := resourcesPattern.FindStringSubmatch(data)
	if len(resourcesSection) < 2 {
		return nil, errors.New("resources section not found or empty")
	}

	linePattern := regexp.MustCompile(`\| \[([^\]]+)\]\([^\)]+\) \| [^\|]+\|`)
	matches := linePattern.FindAllStringSubmatch(resourcesSection[1], -1)

	for _, match := range matches {
		if len(match) > 1 {
			resources = append(resources, strings.TrimSpace(match[1]))
		}
	}

	return resources, nil
}


func extractTerraformResources() ([]string, error) {
	var resources []string

	// make use of builtin github environment variable
	mainPath := filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "caller", "main.tf")
	specificResources, err := extractFromFilePath(mainPath)
	if err != nil {
		return nil, err
	}
	resources = append(resources, specificResources...)

	modulesPath := filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "caller", "modules")
	modulesResources, err := extractRecursively(modulesPath)
	if err != nil {
		return nil, err
	}
	resources = append(resources, modulesResources...)

	return resources, nil
}

func extractRecursively(dirPath string) ([]string, error) {
	var resources []string
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		return resources, nil
	} else if err != nil {
		return nil, err
	}
	err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && filepath.Base(path) == "main.tf" {
			fileResources, err := extractFromFilePath(path)
			if err != nil {
				return err
			}
			resources = append(resources, fileResources...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resources, nil
}

func extractFromFilePath(filePath string) ([]string, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("error reading file %s: %v", filePath, err)
	}

	var resources []string

	// use regex to find resource and data blocks
	resourceRegex := regexp.MustCompile(`(?m)^resource\s+"(\w+)"\s+"`)
	dataRegex := regexp.MustCompile(`(?m)^data\s+"(\w+)"\s+"`)

	for _, match := range resourceRegex.FindAllStringSubmatch(string(content), -1) {
		resources = append(resources, match[1])
	}

	for _, match := range dataRegex.FindAllStringSubmatch(string(content), -1) {
		resources = append(resources, match[1])
	}

	return resources, nil
}

func formatError(format string, args ...interface{}) error {
	return fmt.Errorf(format, args...)
}

func parseHeaders(headerRow string) []string {
	headers := strings.Split(strings.TrimSpace(headerRow), "|")
	for i, header := range headers {
		headers[i] = strings.TrimSpace(header)
	}
	return headers
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i, v := range a {
		if v != b[i] {
			return false
		}
	}
	return true
}

func findMissingItems(a, b []string) []string {
	mb := make(map[string]struct{}, len(b))
	for _, x := range b {
		mb[x] = struct{}{}
	}
	var diff []string
	for _, x := range a {
		if _, found := mb[x]; !found {
			diff = append(diff, x)
		}
	}
	return diff
}

func compareColumns(header string, expected, actual []string) error {
	var mismatchedColumns []string
	for i := 0; i < len(expected) || i < len(actual); i++ {
		expectedCol := ""
		actualCol := ""
		if i < len(expected) {
			expectedCol = expected[i]
		}
		if i < len(actual) {
			actualCol = actual[i]
		}
		if expectedCol != actualCol {
			mismatchedColumns = append(mismatchedColumns, fmt.Sprintf("expected '%s', found '%s'", expectedCol, actualCol))
		}
	}
	return formatError("table under header: %s has incorrect column names:\n  %s", header, strings.Join(mismatchedColumns, "\n  "))
}

func TestMarkdown(t *testing.T) {
	readmePath := "README.md"
	if envPath := os.Getenv("README_PATH"); envPath != "" {
		readmePath = envPath
	}

	validator, err := NewMarkdownValidator(readmePath)
	if err != nil {
		t.Fatalf("Failed to create validator: %v", err)
	}

	errors := validator.Validate()
	if len(errors) > 0 {
		for _, err := range errors {
			t.Errorf("Validation error: %v", err)
		}
	}
}
