package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/adminapi"
	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tunnel"
)

// flagWasSet reports whether name was passed explicitly on the command line
// (as opposed to left at its default), which the std flag package does not
// expose directly.
func flagWasSet(flags *flag.FlagSet, name string) bool {
	set := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// defaultFleetTimeout bounds each per-worker fleet request (tunnel open + admin
// call). It is injectable for tests through fleetOptions.
const defaultFleetTimeout = 10 * time.Second

// fleetManager is the subset of *tunnel.Manager the fleet commands use. It is
// an interface so tests can inject a fake whose Transport reaches an httptest
// admin server without spawning ssh.
type fleetManager interface {
	Transport(name string) (http.RoundTripper, error)
	States() map[string]tunnel.State
	Close() error
}

// newFleetManager is the seam for building a fleet manager from resolved
// deployments. It defaults to the real SSH-backed tunnel.Manager and is
// overridden in tests with a fake-transport manager. Replacing this var (rather
// than threading a factory through every command) keeps the fleet flag plumbing
// identical across status/logs/trace.
var newFleetManager = func(deployments []deploy.Deployment, opts tunnel.Options) (fleetManager, error) {
	return tunnel.NewManager(deployments, opts)
}

// fleetSelector captures the parsed --worker/--all flags shared by the three
// commands.
type fleetSelector struct {
	worker string // single target name
	all    bool   // fan out across the whole inventory
}

// active reports whether the command should run in fleet mode at all.
func (s fleetSelector) active() bool { return s.all || strings.TrimSpace(s.worker) != "" }

// validate enforces the mutually-exclusive flag rules. urlSet reports whether
// --url was passed explicitly (vs. left at its default), which conflicts with
// fleet mode.
func (s fleetSelector) validate(urlSet bool) error {
	if s.all && strings.TrimSpace(s.worker) != "" {
		return fmt.Errorf("%w: --worker and --all are mutually exclusive", ErrUsage)
	}
	if s.active() && urlSet {
		return fmt.Errorf("%w: --url cannot be combined with --worker/--all (fleet targets come from the deployments inventory)", ErrUsage)
	}
	return nil
}

// fleetOptions carries the resolved inputs for a fleet run.
type fleetOptions struct {
	// token is the operator-supplied --token (may be empty); it flows to
	// tunnel.Options.Token, never to the adminapi.Client header.
	token string
	// timeout bounds each per-worker request; zero means defaultFleetTimeout.
	timeout time.Duration
}

// resolveFleetTargets loads the inventory and returns the deployments the
// selector targets. --worker resolves a single named entry; --all returns the
// whole inventory. An empty inventory or unknown worker is a usage-level error.
func resolveFleetTargets(sel fleetSelector) ([]deploy.Deployment, error) {
	path, err := deploy.InventoryPath()
	if err != nil {
		return nil, err
	}
	inv, err := deploy.LoadInventory(path)
	if err != nil {
		return nil, err
	}

	// Collapse duplicate (name,host) rows to one entry per name: the tunnel
	// manager keys on name and rejects duplicates. The first row per name wins
	// (inventory is sorted by name,host).
	byName := make(map[string]deploy.Deployment)
	order := make([]string, 0, len(inv.Deployments))
	for _, d := range inv.Deployments {
		if _, seen := byName[d.Name]; seen {
			continue
		}
		byName[d.Name] = d
		order = append(order, d.Name)
	}

	if sel.all {
		if len(order) == 0 {
			return nil, fmt.Errorf("%w: no deployments recorded (%s); deploy a worker first", ErrUsage, path)
		}
		sort.Strings(order)
		out := make([]deploy.Deployment, 0, len(order))
		for _, name := range order {
			out = append(out, byName[name])
		}
		return out, nil
	}

	name := strings.TrimSpace(sel.worker)
	d, ok := byName[name]
	if !ok {
		return nil, fmt.Errorf("%w: no recorded deployment named %q (run `ouvrier fleet ls`)", ErrUsage, name)
	}
	return []deploy.Deployment{d}, nil
}

// fleetResult is one worker's outcome: its rendered output block and any error.
type fleetResult struct {
	name   string
	output string
	err    error
}

// fleetCall is the per-worker admin operation: it receives a Client bound to
// the worker's tunnel transport and a writer to render into. It must not set
// its own Authorization header (the tunnel transport injects the token).
type fleetCall func(ctx context.Context, name string, client *adminapi.Client, out io.Writer) error

// runFleet executes call against every target, concurrently when more than one,
// each under its own timeout. It prefixes every worker's output block with the
// worker name, prints successes and failures in stable name order, and returns
// a non-nil error if ANY target failed (successes are still printed). All errors
// are token-masked before they reach the writer.
func (app *App) runFleet(ctx context.Context, targets []deploy.Deployment, opts fleetOptions, call fleetCall) error {
	// Pin ssh host keys against <cwd>/ouvrier.known_hosts, the same project-root
	// convention every deploy invocation uses.
	mgr, err := newFleetManager(targets, tunnel.Options{Dir: ".", Token: strings.TrimSpace(opts.token)})
	if err != nil {
		return maskFleetErr(err, opts.token)
	}
	defer mgr.Close()

	timeout := opts.timeout
	if timeout <= 0 {
		timeout = defaultFleetTimeout
	}

	results := make([]fleetResult, len(targets))
	var wg sync.WaitGroup
	for i, d := range targets {
		wg.Add(1)
		go func(i int, d deploy.Deployment) {
			defer wg.Done()
			results[i] = runFleetOne(ctx, mgr, d, timeout, call)
		}(i, d)
	}
	wg.Wait()

	states := mgr.States()
	sort.SliceStable(results, func(i, j int) bool { return results[i].name < results[j].name })

	var failed int
	for _, r := range results {
		st := states[r.name]
		fmt.Fprintf(app.out, "=== %s [%s] ===\n", r.name, st.Status)
		if r.err != nil {
			failed++
			fmt.Fprintf(app.out, "  error: %s\n", maskFleetErr(r.err, opts.token).Error())
			continue
		}
		app.writePrefixed(r.output, r.name)
	}

	if failed > 0 {
		return fmt.Errorf("%w: %d of %d worker(s) failed", errFleetPartial, failed, len(results))
	}
	return nil
}

// errFleetPartial marks a fleet run where at least one worker failed. The
// successes were still printed; the nonzero exit signals partial failure.
var errFleetPartial = errors.New("fleet command had failures")

// runFleetOne opens the worker's tunnel transport, builds a tokenless adminapi
// Client over it, and runs call under a per-worker timeout.
func runFleetOne(ctx context.Context, mgr fleetManager, d deploy.Deployment, timeout time.Duration, call fleetCall) fleetResult {
	res := fleetResult{name: d.Name}
	rt, err := mgr.Transport(d.Name)
	if err != nil {
		res.err = err
		return res
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Empty token: the tunnel transport injects the in-memory fetched token, so
	// the Client must NOT set its own Authorization header. The base URL host is
	// ignored by the transport; the worker name keeps error messages readable.
	client := adminapi.NewClient(&http.Client{Transport: rt}, "http://"+d.Name, "")

	var buf strings.Builder
	res.err = call(cctx, d.Name, client, &buf)
	res.output = buf.String()
	return res
}

// writePrefixed renders block with "<name>  " in front of every non-empty line
// so fleet output is unambiguous when several workers interleave.
func (app *App) writePrefixed(block, name string) {
	if block == "" {
		return
	}
	lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
	for _, line := range lines {
		if line == "" {
			fmt.Fprintf(app.out, "%s\n", name)
			continue
		}
		fmt.Fprintf(app.out, "%s  %s\n", name, line)
	}
}

// maskFleetErr masks any operator-supplied --token from an error before it is
// surfaced, defense-in-depth on top of the tunnel manager's own masking.
func maskFleetErr(err error, token string) error {
	if err == nil {
		return nil
	}
	return deploy.MaskTokenErr(err, strings.TrimSpace(token))
}
