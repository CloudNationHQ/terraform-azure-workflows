package main

import (
	"github.com/cloudnationhq/az-cn-go-validor"
	"log"
	"os"
)

func main() {
	examplesPath := "examples"
	if len(os.Args) > 1 {
		examplesPath = os.Args[1]
	}

	if err := validor.ValidateLocalChanges(examplesPath); err != nil {
		log.Fatal(err)
	}
}
