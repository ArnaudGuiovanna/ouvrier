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

const newHelp = `Scaffold a new Ouvrier project.

Usage: ouvrier new [flags]

With no flags, ouvrier opens the Bubble Tea wizard.

Options:
      --name string      Project name
      --trigger string   Trigger, for example "POST /tickets"
      --model string     Model ID, for example anthropic/claude-sonnet-4-6
      --dir string       Parent directory for the project (default ".")
      --yes              Confirm non-interactive scaffold
  -h, --help             Show this help message
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
