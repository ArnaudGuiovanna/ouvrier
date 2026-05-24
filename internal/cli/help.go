package cli

import (
	"fmt"
	"io"
)

const rootHelp = `Ouvrier - Workers for your APIs.

Usage: ouvrier <command> [flags]

Commands:
  new       Scaffold a new Ouvrier project
  build     Compile an Ouvrier project to a binary
  show      Summarize the current project's pip.yaml
  status    Show health and counters for a running worker
  logs      List the last N traced executions of a running worker
  trace     Print the full event timeline for one execution
  version   Print the ouvrier CLI version

Run "ouvrier <command> --help" for command details.
`

const newHelp = `Scaffold a new Ouvrier project.

Usage: ouvrier new [flags]

With no flags, ouvrier opens a Bubble Tea preview. Interactive generation is
still a v0.1 backlog item; use --yes with flags to scaffold today.

Options:
      --name string      Project name
      --trigger string   HTTP trigger, for example "POST /tickets"
      --model string     Model ID, for example anthropic/claude-sonnet-4-6
      --dir string       Parent directory for the project (default ".")
      --yes              Confirm non-interactive scaffold
  -h, --help             Show this help message
`

const versionHelp = `Print the ouvrier CLI version.

Usage: ouvrier version
`

const buildHelp = `Compile an Ouvrier project to a binary.

Usage: ouvrier build [flags]

Reads pip.yaml from the project directory to derive the binary name and
invokes the host go toolchain. Secrets in .env are never embedded.

Options:
      --dir string       Project directory (default ".")
      --output string    Output binary path (default "./bin/<name>")
      --target string    Cross-compile target as GOOS/GOARCH (e.g. linux/amd64)
      --static           CGO-disabled build with -ldflags="-s -w"
  -h, --help             Show this help message
`

const showHelp = `Summarize the current project's pip.yaml.

Usage: ouvrier show [flags]

Options:
      --dir string   Project directory containing pip.yaml (default ".")
  -h, --help         Show this help message
`

const statusHelp = `Show health and counters for a running worker.

Usage: ouvrier status [flags]

Options:
      --url string     Worker base URL (default "http://localhost:8080")
      --token string   Admin bearer token (defaults to $PIP_ADMIN_TOKEN)
  -h, --help           Show this help message
`

const logsHelp = `List the last N traced executions of a running worker.

Usage: ouvrier logs [flags]

Options:
      --url string     Worker base URL (default "http://localhost:8080")
      --token string   Admin bearer token (defaults to $PIP_ADMIN_TOKEN)
      --last int       Number of executions to fetch (default 20)
  -h, --help           Show this help message
`

const traceHelp = `Print the full event timeline for one execution.

Usage: ouvrier trace [flags] <exec-id>

Options:
      --url string     Worker base URL (default "http://localhost:8080")
      --token string   Admin bearer token (defaults to $PIP_ADMIN_TOKEN)
  -h, --help           Show this help message
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

func printBuildHelp(w io.Writer) {
	fmt.Fprint(w, buildHelp)
}

func printShowHelp(w io.Writer) {
	fmt.Fprint(w, showHelp)
}

func printStatusHelp(w io.Writer) {
	fmt.Fprint(w, statusHelp)
}

func printLogsHelp(w io.Writer) {
	fmt.Fprint(w, logsHelp)
}

func printTraceHelp(w io.Writer) {
	fmt.Fprint(w, traceHelp)
}
