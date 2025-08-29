package main

import (
	"github.com/cloudnationhq/az-cn-go-validor"
	"log"
	"os"
	"path/filepath"
)

func main() {
	// Change to repo directory for module detection
	if repoPath := os.Getenv("REPO_PATH"); repoPath != "" {
		// Go up one level to get to the actual repo directory
		actualRepoPath := filepath.Dir(repoPath)
		if err := os.Chdir(actualRepoPath); err != nil {
			log.Fatal("Failed to change to repo directory:", err)
		}
	}

	examplesPath := os.Getenv("EXAMPLES_PATH")
	if examplesPath == "" {
		examplesPath = "examples"
	}

	if err := validor.ValidateLocalChanges(examplesPath); err != nil {
		log.Fatal(err)
	}
}
