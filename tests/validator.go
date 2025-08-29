package main

import (
	"github.com/cloudnationhq/az-cn-go-validor"
	"log"
	"os"
)

func main() {
	examplesPath := os.Getenv("EXAMPLES_PATH")
	if examplesPath == "" {
		examplesPath = "examples"
	}

	if err := validor.ValidateLocalChanges(examplesPath); err != nil {
		log.Fatal(err)
	}
}
