package main

import (
	"github.com/cloudnationhq/az-cn-go-markparsr"
	"testing"
)

func TestReadmeValidation(t *testing.T) {

	// Use functional options pattern
	validator, err := markparsr.NewReadmeValidator(
		markparsr.WithAdditionalSections("Goals", "Non-Goals",
			"Features", "Authors", "Contributing", "Testing", "Notes", "References"),
		markparsr.WithAdditionalFiles("CONTRIBUTING.md", "TESTING.md",
			"CODE_OF_CONDUCT.md", "SECURITY.md"),
	)

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
