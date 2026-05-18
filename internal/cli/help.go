package cli

import (
	"fmt"
	"io"
)

const rootHelp = `Ouvrier - Workers for your APIs.

Usage: ouvrier <command> [flags]

Commands:
  new       Scaffold a new Ouvrier project
  version   Print the ouvrier CLI version

Run "ouvrier <command> --help" for command details.
`

const newHelp = `Scaffold a new Ouvrier project with the Bubble Tea wizard.

Usage: ouvrier new [flags]

Options:
  -h, --help   Show this help message
`

const versionHelp = `Print the ouvrier CLI version.

Usage: ouvrier version
`

func printRootHelp(w io.Writer) {
	fmt.Fprint(w, rootHelp)
}

func printNewHelp(w io.Writer) {
	fmt.Fprint(w, newHelp)
}

func printVersionHelp(w io.Writer) {
	fmt.Fprint(w, versionHelp)
}
