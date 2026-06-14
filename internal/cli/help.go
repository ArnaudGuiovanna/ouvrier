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
  deploy    Ship the project to a deploy environment or host over SSH, or build a container image
  server    Manage trusted deploy hosts (trust pins SSH host keys)
  fleet     Inspect or prune the recorded deployments inventory (ls|rm)
  console   Start the loopback web console over the federated admin APIs
  operate   Open the local agentic worker-builder cockpit
  state     Manage the worker's durable state backend (migrate)
  version   Print the ouvrier CLI version

Run "ouvrier <command> --help" for command details.
`

const operateHelp = `Open the local agentic worker-builder cockpit.

Usage: ouvrier operate [flags]
       ouvrier operate --print "create a worker that receives POST /tickets"
       ouvrier operate --mode json --prompt "review this worker"
       ouvrier operate --mode rpc
       ouvrier operate create-worker --yes --name <name> --trigger <trigger> --model <model> [flags]
       ouvrier operate patch --goal "<change>" [flags]
       ouvrier operate fix-worker [flags]
       ouvrier operate review-worker [flags]
       ouvrier operate audit [flags]
       ouvrier operate build [flags]
       ouvrier operate transfer --env <name> [flags]

The interactive cockpit is a prompt-first agent harness specialized for
manufacturing Ouvrier workers. Type a goal, review the visible tool transcript,
let the harness scaffold/patch/audit/build/transfer, and keep workers as normal
Go projects. Codex is used only as a local driver; Ouvrier never stores Codex
credentials.

Options:
      --dir string          Worker/project directory (default ".")
      --agent string        Agent driver: codex or manual (default "codex")
      --codex-mode string   Codex transport: auto, exec, or app-server (default "auto")
      --session string      Resume a local operate session
      --goal string         Pre-fill the first builder prompt
      --prompt string       Run one prompt without opening the TUI
      --print               Run prompt mode and print the transcript
      --mode string         tui, print, json, or rpc (default "tui")
      --json                Shortcut for --mode json
      --target string       Build/deploy target, e.g. linux/amd64
      --allow-failed        Override audit/review gates for build or transfer
  -h, --help                Show this help message

Review worker mode:
      --scope string        whole_worker, changed_files, tool, pipeline,
                            governance_security, deploy_readiness, failing_trace
      --subject string      Optional review subject, e.g. tool name or pipeline

Transfer mode:
      --env string          deploy.<env> from pip.yaml
      --env-file string     Env file passed to deploy
      --keep int            Releases to keep on the host

Create worker mode:
      --name string         Project name
      --trigger string      Initial worker trigger
      --model string        Initial provider/model
      --yes                 Confirm file creation
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
      --token string   Admin bearer token (defaults to $OUVRIER_ADMIN_TOKEN)
      --worker string  Target a deployed worker by name over a one-shot tunnel
      --all            Fan out across every deployed worker (partial failures
                       are reported per worker; the command exits nonzero if any
                       worker failed)
  -h, --help           Show this help message

In fleet mode (--worker/--all) the worker base URL and token come from the
deployments inventory and the SSH tunnel; --url cannot be combined with
--worker/--all, and --worker and --all are mutually exclusive. Fleet status also
surfaces each worker's tunnel state and cron_leases.
`

const logsHelp = `List the last N traced executions of a running worker.

Usage: ouvrier logs [flags]

Options:
      --url string     Worker base URL (default "http://localhost:8080")
      --token string   Admin bearer token (defaults to $OUVRIER_ADMIN_TOKEN)
      --last int       Number of executions to fetch (default 20)
      --worker string  Target a deployed worker by name over a one-shot tunnel
      --all            Fan out across every deployed worker (partial failures
                       are reported per worker; nonzero exit if any failed)
  -h, --help           Show this help message

In fleet mode (--worker/--all) targets come from the deployments inventory over
SSH tunnels; --url cannot be combined with --worker/--all, and --worker and
--all are mutually exclusive.
`

const traceHelp = `Print the full event timeline for one execution.

Usage: ouvrier trace [flags] <exec-id>

Options:
      --url string     Worker base URL (default "http://localhost:8080")
      --token string   Admin bearer token (defaults to $OUVRIER_ADMIN_TOKEN)
      --worker string  Target a deployed worker by name over a one-shot tunnel
      --all            Fan out across every deployed worker (partial failures
                       are reported per worker; nonzero exit if any failed)
  -h, --help           Show this help message

In fleet mode (--worker/--all) targets come from the deployments inventory over
SSH tunnels; --url cannot be combined with --worker/--all, and --worker and
--all are mutually exclusive.
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
      --addr string   Address override exposed via OUVRIER_ADDR (default ":8080")
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

const deployHelp = `Ship a project to its deploy environment, a host, or a container image.

Usage: ouvrier deploy <env> [flags]
       ouvrier deploy ssh --host HOST [flags]
       ouvrier deploy rollback <env> [flags]
       ouvrier deploy docker [flags]

<env> names a committed pip.yaml deploy.<env> block (the server registry),
e.g. "ouvrier deploy staging". "deploy ssh" runs the same release flow
against a single explicit --host, bypassing the registry. "deploy rollback"
repoints each host at the previous release recorded in its deploys.log — no
build. "deploy docker" builds a distroless OCI image instead.

Run "ouvrier deploy ssh --help", "ouvrier deploy rollback --help", or
"ouvrier deploy docker --help" for details.
`

const deploySSHHelp = `Deploy the project to remote Linux hosts over plain SSH.

Usage: ouvrier deploy <env> [flags]
       ouvrier deploy ssh --host HOST [flags]

<env> resolves hosts and defaults (port/path/service/identity/sandbox) from
the committed pip.yaml deploy.<env> block; "deploy ssh --host user@host" is
the registry-bypass alias for one explicit host. Both run the same flow:

  1. Local preflight: resolve the env file (.env.<env>, .env, --env-file or
     OUVRIER_DEPLOY_ENV_FILE), validate pip.yaml env.required plus
     OUVRIER_ADMIN_TOKEN, refuse git-tracked env files, then build a static
     binary (--target, default linux/amd64) and compute its sha256
  2. Remote preflight: passwordless-sudo probe, systemd check, create the
     ouvrier-<name> system user, create the release layout, take the
     .deploy.lock flock
  3. Upload the release into <path>/releases/<ts>-<sha>/ (binary,
     RELEASE.json, skills/ assets); verify the remote sha256; chmod 0755
  4. Ship the env file atomically to <path>/shared/.env (root:svc 0640),
     appending OUVRIER_ADMIN_ADDR=127.0.0.1:9090 when it sets none; a
     non-loopback admin addr is refused without --allow-shared-admin
  5. Install the systemd unit only when it changed (+ daemon-reload), enable
  6. Atomically repoint <path>/current; sudo systemctl restart
  7. Health gate: on-host curl of 127.0.0.1:<admin port>/admin/health, 10
     attempts over ~30s; the token travels via curl stdin config, never argv
  8. Success: append deploys.log, prune to --keep releases, record the
     deploy in the local inventory. Failure: dump journalctl, roll back to
     the previous release, restart, re-check; a first deploy stops the unit

Multiple hosts deploy sequentially and abort on the first failure (with a
loud mixed-version warning). Deploying to "prod"/"production" asks for
confirmation unless --yes.

Options:
      --host string         Explicit target (user@host); required for "deploy ssh",
                            narrows "deploy <env>" to one host
      --user string         SSH user (overrides any user@ in the host)
      --port int            SSH port (default: ssh's default, usually 22)
      --path string         Remote install root (default "/opt/ouvrier/<name>")
      --service string      systemd unit name (default "ouvrier-<name>")
      --dir string          Project directory containing pip.yaml (default ".")
      --env-file string     Dotenv file to ship (default ".env.<env>" then ".env";
                            also OUVRIER_DEPLOY_ENV_FILE)
      --identity string     SSH identity file passed as -i to ssh/scp (agent-less
                            CI; pip.yaml deploy.<env> identity is the default)
      --target string       Cross-compile target GOOS/GOARCH (default "linux/amd64")
      --keep int            Releases to keep on the host after pruning (default 5)
      --yes                 Skip the prod/production confirmation prompt (CI)
      --allow-shared-admin  Permit an env file whose OUVRIER_ADMIN_ADDR binds the
                            admin API beyond loopback (off by default)
      --unit-sandbox string Systemd hardening: "on" (default) or "off" (escape
                            hatch, same as pip.yaml deploy sandbox: off)
      --print-sudoers       Print the least-privilege sudoers snippet for this
                            project's deploy flow and exit (no host needed)
  -h, --help                Show this help message

Every target host must first be pinned with "ouvrier server trust <host>":
every ssh/scp invocation runs with -o UserKnownHostsFile=ouvrier.known_hosts
-o StrictHostKeyChecking=yes -o BatchMode=yes and password/keyboard-interactive
authentication disabled. A deploy against an unpinned host fails before any
remote command, and a changed host key is a hard error pointing at
"ouvrier server trust --rotate".

The admin token is read from the shipped env file (never from a flag), is fed
to the remote health probe over stdin (never argv), and is masked in all
output.
`

const deployRollbackHelp = `Roll hosts back to the previous release from their deploys.log ledger.

Usage: ouvrier deploy rollback <env> [flags]
       ouvrier deploy rollback --host HOST [flags]

<env> resolves hosts and defaults from the committed pip.yaml deploy.<env>
block exactly like "ouvrier deploy <env>"; --host targets one explicit host.
Rollback never builds or uploads anything. Per host it:

  1. Takes the same <path>/.deploy.lock as a deploy (rollback mutates the
     current symlink), released on every exit path
  2. Reads the LAST <path>/deploys.log entry and resolves the release that
     entry replaced (recorded as previous=<target> at deploy time — rollback
     never guesses from timestamps)
  3. Verifies that release directory still exists, BEFORE touching current
  4. Atomically repoints <path>/current; sudo systemctl restart
  5. Runs the same health gate as deploy: on-host curl of
     127.0.0.1:<admin port>/admin/health, 10 attempts over ~30s; the token
     travels via curl stdin config, never argv, and is masked in all output
  6. Appends a distinguishable "rollback" entry to deploys.log and records
     the rollback in the local inventory (result "rollback-ok")

It refuses with an actionable error — leaving current untouched — when there
is no deploy history, when the last deploy recorded no previous release (a
first deploy), or when the previous release directory was pruned by --keep;
redeploy a known-good revision with "ouvrier deploy <env>" instead.

The host's shared/.env is intentionally NOT rolled back: the latest shipped
secrets stay in place (per-release env snapshotting is a pending design
decision). The local env file (.env.<env>, .env, --env-file or
OUVRIER_DEPLOY_ENV_FILE) is read only for the OUVRIER_ADMIN_TOKEN and
OUVRIER_ADMIN_ADDR the health gate needs, so its token must match the one
already deployed to the host.

Multiple hosts roll back sequentially and abort on the first failure (with a
loud mixed-version warning). Rolling back "prod"/"production" asks for
confirmation unless --yes.

Options:
      --host string         Explicit target (user@host); required without <env>,
                            narrows "deploy rollback <env>" to one host
      --user string         SSH user (overrides any user@ in the host)
      --port int            SSH port (default: ssh's default, usually 22)
      --path string         Remote install root (default "/opt/ouvrier/<name>")
      --service string      systemd unit name (default "ouvrier-<name>")
      --dir string          Project directory containing pip.yaml (default ".")
      --env-file string     Dotenv file read for the health-gate token/addr
                            (default ".env.<env>" then ".env";
                            also OUVRIER_DEPLOY_ENV_FILE)
      --identity string     SSH identity file passed as -i to ssh (agent-less
                            CI; pip.yaml deploy.<env> identity is the default)
      --yes                 Skip the prod/production confirmation prompt (CI)
      --allow-shared-admin  Permit an env file whose OUVRIER_ADMIN_ADDR binds the
                            admin API beyond loopback (off by default)
  -h, --help                Show this help message

Every target host must be pinned with "ouvrier server trust <host>", exactly
like a deploy.
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

const serverHelp = `Manage trusted deploy hosts.

Usage: ouvrier server <trust> [flags]

Subcommands:
  trust   Pin a host's SSH public keys into the committed ouvrier.known_hosts

Run "ouvrier server <subcommand> --help" for details.
`

const serverTrustHelp = `Pin a remote host's SSH public keys into ouvrier.known_hosts.

Usage: ouvrier server trust <host> [flags]

Runs ssh-keyscan against the host, displays the SHA256 key fingerprint, and
after confirmation appends every scanned key line to ouvrier.known_hosts at
the project root. Host public keys are not secrets: commit the file so the
trust decision is shared by the whole team and CI, and every
"ouvrier deploy ssh" verifies the host against it (StrictHostKeyChecking=yes).

The fingerprint shown and checked by --fingerprint is the host's ed25519 key
when it offers one, otherwise the first scanned key; all scanned key types
are pinned either way. Verify it out-of-band (e.g. "ssh-keygen -lf
/etc/ssh/ssh_host_ed25519_key.pub" on the server console).

Trusting an already-pinned host with the same key is a no-op. If the host's
key has changed, the command refuses unless --rotate is given, which replaces
the pinned entries with the fresh scan.

Options:
      --fingerprint string  Expected SHA256 fingerprint (with or without the
                            "SHA256:" prefix) for non-interactive use (CI);
                            a mismatch aborts and writes nothing
      --rotate              Replace existing pinned keys for this host
      --port int            SSH port; non-default ports pin "[host]:port"
      --dir string          Project root holding ouvrier.known_hosts (default ".")
  -h, --help                Show this help message
`

const fleetHelp = `Inspect or prune the recorded deployments inventory.

Usage: ouvrier fleet <ls|rm> [flags]

The inventory at ~/.config/ouvrier/deployments.json records one entry per
deployed worker and host. It is a disposable cache for tooling — the live
/admin/health endpoint is truth — and never contains secrets. Override its
location with OUVRIER_FLEET_PATH (full path) or OUVRIER_CONFIG_DIR.

Subcommands:
  ls   List recorded deployments
  rm   Remove recorded deployments for a worker

Run "ouvrier fleet <subcommand> --help" for details.
`

const fleetLsHelp = `List recorded deployments.

Usage: ouvrier fleet ls

Prints one line per recorded deployment (name, host, service, deploy time,
result). An empty inventory is not an error.

Options:
  -h, --help   Show this help message
`

const fleetRmHelp = `Remove recorded deployments for a worker.

Usage: ouvrier fleet rm <name> [flags]

Removes the inventory entries for the named worker. This only edits the local
inventory file; it never touches the remote host.

Options:
      --host string   Only remove the entry for this host
  -h, --help          Show this help message
`

const stateHelp = `Manage the worker's durable state backend.

Usage: ouvrier state <migrate> [flags]

Subcommands:
  migrate   Apply pending schema migrations to the configured state backend

Run "ouvrier state <subcommand> --help" for details.
`

const stateMigrateHelp = `Apply pending schema migrations to the configured state backend.

Usage: ouvrier state migrate

Reads the same environment the worker uses:

  OUVRIER_STATE_BACKEND   sqlite (default) or postgres
  OUVRIER_STATE_PATH      SQLite database path (default ".ouvrier/state.db")
  OUVRIER_STATE_DSN       Postgres connection string (required for postgres)

Postgres migrations run inside one transaction serialized by an advisory
lock, so concurrent invocations are safe; SQLite migrations stamp PRAGMA
user_version. The command prints each schema version it applies and is a
no-op when the schema is already current.

Run this with a DDL-capable role when the worker itself connects with a
DML-only role and OUVRIER_STATE_MIGRATE=off. The DSN is secret-bearing and
is never printed.

Options:
  -h, --help   Show this help message
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

func printDeployRollbackHelp(w io.Writer) {
	fmt.Fprint(w, deployRollbackHelp)
}

func printDeployDockerHelp(w io.Writer) {
	fmt.Fprint(w, deployDockerHelp)
}

func printServerHelp(w io.Writer) {
	fmt.Fprint(w, serverHelp)
}

func printServerTrustHelp(w io.Writer) {
	fmt.Fprint(w, serverTrustHelp)
}

func printFleetHelp(w io.Writer) {
	fmt.Fprint(w, fleetHelp)
}

func printFleetLsHelp(w io.Writer) {
	fmt.Fprint(w, fleetLsHelp)
}

func printFleetRmHelp(w io.Writer) {
	fmt.Fprint(w, fleetRmHelp)
}

func printStateHelp(w io.Writer) {
	fmt.Fprint(w, stateHelp)
}

func printStateMigrateHelp(w io.Writer) {
	fmt.Fprint(w, stateMigrateHelp)
}
