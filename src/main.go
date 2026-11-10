package main

import (
	"cloud/src/app"
	"log"
	"os"
)

// version is set at build time: -ldflags "-X main.version=1.0.0".
var version = "dev"

func main() {
	if err := app.Run(os.Args[1:], version); err != nil {
		log.Fatal(err)
	}
}
