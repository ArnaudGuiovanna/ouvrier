package cli

import (
	"fmt"
	"io"
)

const rootHelp = `Ouvrier - Workers for your APIs.

Usage: ouvrier <command> [flags]

Commands:
  new       Scaffold a new Ouvrier project
  add       Add an agent, tool, or skill to an existing project
  dev       Run the worker locally (go run .) until interrupted
  build     Compile an Ouvrier project to a binary
  show      Summarize the current project's pip.yaml
  status    Show health and counters for a running worker
  logs      List the last N traced executions of a running worker
  trace     Print the full event timeline for one execution
  deploy    Ship the project to a remote host (ssh) or build a container image (docker)
  version   Print the ouvrier CLI version

Run "ouvrier <command> --help" for command details.
`

const newHelp = `Scaffold a new Ouvrier project.

Usage: ouvrier new [flags]

With no flags, ouvrier opens the Bubble Tea project wizard. Use --yes with
flags for non-interactive scaffolding.

Options:
      --name string      Project name
      --trigger string   Trigger: http "POST /tickets", cron "0 6 * * *",
                         webhook "webhook github", or stream "stream kafka://tickets"
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
      --json         Print a machine-readable JSON summary
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

const addHelp = `Add an agent, trigger, tool, or skill to an existing Ouvrier project.

Usage: ouvrier add <agent|trigger|tool|skill> [flags]

Subcommands:
  agent     Append a new ovr.Pipe to main.go
  trigger   Append a new trigger pipeline to main.go
  tool      Generate a tool stub and register it in the first Pipe
  skill     Create a new SKILL.md and register it in the first Pipe

Run "ouvrier add <subcommand> --help" for details.
`

const addAgentHelp = `Append a new ovr.Pipe(...) declaration to main.go.

Usage: ouvrier add agent --name NAME --model MODEL [flags]

Options:
      --name string     Agent name shown in traces (required)
      --model string    Model ID as provider/name (required)
      --goal string     Goal sentence describing what this agent does
      --dir string      Project directory containing pip.yaml (default ".")
  -h, --help            Show this help message

The command refuses to run unless pip.yaml exists in --dir. The new Pipe is
inserted immediately after the existing ovr.Pipe(...) line, or before the
terminal ovr.Reply/ovr.Push/ovr.Sink if no other Pipe was found. If neither
anchor is detected the command refuses to edit main.go.
`

const addTriggerHelp = `Append a new trigger pipeline to main.go.

Usage: ouvrier add trigger --trigger TRIGGER [flags]

Options:
      --trigger string   HTTP route, cron expression, webhook provider, or stream URI
      --model string     Model ID as provider/name (default: first model in main.go, then anthropic/claude-sonnet-4-6)
      --goal string      Goal sentence for the generated Pipe
      --dir string       Project directory containing pip.yaml (default ".")
  -h, --help             Show this help message

Examples:
  ouvrier add trigger --trigger "cron @every 1h"
  ouvrier add trigger --trigger "webhook github" --model openai/gpt-4.1-mini
  ouvrier add trigger --trigger "stream kafka://tickets"

The command appends a full ovr.From(...), ovr.Pipe(...), and terminal node
inside the existing ovr.Run(...) call. HTTP triggers use a JSON reply;
cron, webhook, and stream triggers use ovr.Sink(ovr.Log()) by default.
`

const addToolHelp = `Generate a Go tool stub and register it in the first Pipe.

Usage: ouvrier add tool --name NAME [flags]

Options:
      --name string         Tool name (Go identifier, e.g. load_ticket)
      --describe string     One-line LLM-facing description
      --readonly            Mark the tool as ReadOnly()
      --side-effecting      Mark the tool as SideEffecting("default")
      --idempotent string   Mark the tool as Idempotent(key); takes a key expression
      --dir string          Project directory containing pip.yaml (default ".")
  -h, --help                Show this help message

A new file tools/<snake_case_name>.go is created with an example Args/Result
pair and a function stub. The command also appends an ovr.Tool(...) line into
the first ovr.Pipe(...) block of main.go. Only one of --readonly,
--side-effecting, or --idempotent may be used.
`

const addSkillHelp = `Create a new SKILL.md and register it in the first Pipe.

Usage: ouvrier add skill --name NAME [flags]

Options:
      --name string          Skill name (kebab-case)
      --description string   One-line description for the SKILL.md frontmatter
      --dir string           Project directory containing pip.yaml (default ".")
  -h, --help                 Show this help message

Creates skills/<name>/SKILL.md with valid frontmatter (name and description)
and a body placeholder. Refuses to overwrite an existing SKILL.md. Also
appends an ovr.Skill("<name>") line into the first ovr.Pipe block in main.go.
`

const devHelp = `Run the worker locally (go run .) with hot reload until interrupted.

Usage: ouvrier dev [flags]

Options:
      --addr string   Address override exposed via PIP_ADDR (default ":8080")
      --dir string    Project directory containing main.go and pip.yaml (default ".")
      --no-reload     Disable hot reload; run "go run ." once
      --no-dotenv     Do not auto-load a local .env into the worker environment
  -h, --help          Show this help message

The dev runner shells out to "go run ." in --dir, streams stdout/stderr, and
forwards SIGINT/SIGTERM to the child process. Hot reload watches *.go files,
tools/, skills/, and pip.yaml (ignoring .ouvrier/ and build artifacts) by
polling mod-times; on a change it gracefully stops the worker and restarts it.
A build or start failure is logged and the watcher keeps running. Pass
--no-reload to run the worker once without watching.

For convenience, dev auto-loads a local <dir>/.env into the worker environment.
The real process environment always wins, so explicitly-set variables are never
overridden, and .env values are never printed. This is dev-only; deployed
binaries are unaffected. Pass --no-dotenv to disable.
`

const deployHelp = `Ship a project to a remote host or build an image for it.

Usage: ouvrier deploy <ssh|docker> [flags]

Subcommands:
  ssh      Build statically, scp the binary and .env, install systemd, restart and health-check
  docker   Generate a distroless Dockerfile, build, optionally push the image

Run "ouvrier deploy <subcommand> --help" for details.
`

const deploySSHHelp = `Deploy the project to a remote Linux host over SSH.

Usage: ouvrier deploy ssh --host HOST [flags]

Pipeline:
  1. ouvrier build --static --target linux/amd64
  2. scp the binary to <path>/bin/<name>.new and .env to <path>/.env (chmod 0600)
  3. Generate <path>/<service>.service and install it; sudo systemctl daemon-reload
  4. Promote .new into place; sudo systemctl restart <service>
  5. Probe http://127.0.0.1:8080<health-url> with curl --max-time 5 on the remote
  6. On health failure: roll back to <name>.previous and restart

Options:
      --host string         Remote host (required); user can be embedded as user@host
      --user string         SSH user (falls back to the host's default)
      --port int            SSH port (default: ssh's default, usually 22)
      --path string         Remote install path (default "/opt/ouvrier/<name>")
      --service string      systemd unit name (default "ouvrier-<name>")
      --dir string          Project directory containing pip.yaml (default ".")
      --health-url string   Health endpoint path or full URL (default "/admin/health")
      --admin-token string  Admin bearer token forwarded to the health probe (masked in logs)
  -h, --help                Show this help message

A local .env in --dir is required. Secrets are never written to the local
logs and the --admin-token value is masked in any printed output.
`

const deployDockerHelp = `Build a distroless OCI image for the project.

Usage: ouvrier deploy docker [flags]

Pipeline:
  1. Render a two-stage Dockerfile next to pip.yaml (distroless static base)
  2. docker build -t <image>:<tag> .
  3. If --push: docker push <image>:<tag>

Options:
      --image string   Image name (default: pip.yaml name)
      --tag string     Image tag (default: pip.yaml version, else "latest")
      --push           Push the built image to its registry
      --force          Overwrite an existing Dockerfile
      --dir string     Project directory containing pip.yaml (default ".")
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

func printAddHelp(w io.Writer) {
	fmt.Fprint(w, addHelp)
}

func printAddAgentHelp(w io.Writer) {
	fmt.Fprint(w, addAgentHelp)
}

func printAddTriggerHelp(w io.Writer) {
	fmt.Fprint(w, addTriggerHelp)
}

func printAddToolHelp(w io.Writer) {
	fmt.Fprint(w, addToolHelp)
}

func printAddSkillHelp(w io.Writer) {
	fmt.Fprint(w, addSkillHelp)
}

func printDevHelp(w io.Writer) {
	fmt.Fprint(w, devHelp)
}

func printDeployHelp(w io.Writer) {
	fmt.Fprint(w, deployHelp)
}

func printDeploySSHHelp(w io.Writer) {
	fmt.Fprint(w, deploySSHHelp)
}

func printDeployDockerHelp(w io.Writer) {
	fmt.Fprint(w, deployDockerHelp)
}
