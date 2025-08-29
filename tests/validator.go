package main

import (
	"github.com/cloudnationhq/az-cn-go-validor"
	"log"
)

func main() {
	if err := validor.ValidateLocalChanges("examples"); err != nil {
		log.Fatal(err)
	}
}
