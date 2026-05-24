package main

import (
	"context"
	"os"

	"github.com/ArnaudGuiovanna/ouvrier/internal/cli"
)

var version = "dev"

func main() {
	app := cli.New(version)
	if err := app.Run(context.Background(), os.Args[1:]); err != nil {
		os.Exit(1)
	}
}
