package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/cloudnationhq/az-cn-go-diffy"
)

type schemaConfig struct {
	Global struct {
		Properties []string `json:"properties"`
	} `json:"global"`
}

func loadSchemaConfig(t *testing.T) schemaConfig {
	t.Helper()

	data, err := os.ReadFile("../.github/schema.json")
	if err != nil {
		t.Fatalf("Failed to read schema config: %v", err)
	}

	var config schemaConfig
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("Failed to parse schema config: %v", err)
	}
	return config
}

func TestTerraformSchemaValidation(t *testing.T) {
	config := loadSchemaConfig(t)

	findings, err := diffy.ValidateSchema(
		diffy.WithGitHubIssueCreation(),
		diffy.WithExcludedProperties(config.Global.Properties...),
		func(opts *diffy.SchemaValidatorOptions) {
			opts.Silent = true
		},
	)
	if err != nil {
		t.Fatalf("Validation failed: %v", err)
	}
	for _, finding := range findings {
		t.Logf("%s", diffy.FormatFinding(finding))
	}
	if len(findings) > 0 {
		t.Errorf("Found %d missing properties/blocks in root or submodules. See logs above.", len(findings))
	}
}
