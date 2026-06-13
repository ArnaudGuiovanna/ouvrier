package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/console"
)

const consoleHelp = `ouvrier console — embedded web UI over the federated admin APIs

Starts a loopback-only web console that layers over the same SSH tunnels and
admin APIs the headless CLI uses (fleet ls, status, logs, trace, deploy). No
admin port is exposed publicly and no token or SSH credential is written to
disk: the per-session token lives only in your browser, and the worker admin
token is fetched into memory by the tunnel manager and injected server-side.

Usage:
  ouvrier console [flags]

Flags:
  --addr <host:port>   bind address (default 127.0.0.1:7333, or OUVRIER_CONSOLE_ADDR)
  --fleet <path>       deployments inventory override (default OUVRIER_FLEET_PATH)
  --token <token>      operator admin token override (default: fetched over SSH)
  --no-open            do not auto-open a browser
  -h, --help           show this help

A non-loopback --addr is refused unless OUVRIER_CONSOLE_INSECURE=1.
`

func printConsoleHelp(w interface{ Write([]byte) (int, error) }) {
	fmt.Fprint(w, consoleHelp)
}

// consoleConfig captures the parsed `ouvrier console` flags.
type consoleConfig struct {
	addr   string
	fleet  string
	token  string
	noOpen bool
}

func (app *App) runConsoleCommand(ctx context.Context, args []string) error {
	if hasHelpFlag(args) {
		printConsoleHelp(app.out)
		return nil
	}
	cfg, err := parseConsoleFlags(args)
	if err != nil {
		return err
	}

	addr := cfg.addr
	if addr == "" {
		addr = strings.TrimSpace(os.Getenv("OUVRIER_CONSOLE_ADDR"))
	}

	srv, err := console.NewServer(console.Options{
		Addr:      addr,
		Dir:       ".",
		FleetPath: cfg.fleet,
		Token:     cfg.token,
	})
	if err != nil {
		return err
	}
	defer srv.Close()

	ln, err := srv.Listen()
	if err != nil {
		return err
	}

	// Render the URL against the concrete bound port (so :0 in scripts and the
	// default both print a clickable link). The token rides in the fragment, so
	// it never reaches the server logs or a Referer header.
	url := consoleURL(ln.Addr(), srv.SessionToken())
	fmt.Fprintf(app.out, "ouvrier console listening on http://%s\n", ln.Addr())
	fmt.Fprintf(app.out, "open: %s\n", url)

	if !cfg.noOpen {
		openBrowser(url, app.errOut)
	}

	// Serve until the context is cancelled (Ctrl-C) or the listener errors.
	errc := make(chan error, 1)
	go func() { errc <- srv.ServeListener(ln) }()
	select {
	case <-ctx.Done():
		_ = ln.Close()
		return nil
	case err := <-errc:
		return err
	}
}

// consoleURL builds the console URL with the token fragment from the bound
// address, normalizing a wildcard bind to loopback for the printed link.
func consoleURL(addr net.Addr, token string) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return fmt.Sprintf("http://%s/#token=%s", addr.String(), token)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s/#token=%s", net.JoinHostPort(host, port), token)
}

func parseConsoleFlags(args []string) (consoleConfig, error) {
	cfg := consoleConfig{}
	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "--no-open":
			cfg.noOpen = true
			i++
		case arg == "--addr", arg == "--fleet", arg == "--token":
			value, advance, err := flagValue(args, i, arg)
			if err != nil {
				return consoleConfig{}, err
			}
			assignConsoleFlag(&cfg, strings.TrimPrefix(arg, "--"), value)
			i += advance
		case strings.HasPrefix(arg, "--addr="):
			cfg.addr = strings.TrimPrefix(arg, "--addr=")
			i++
		case strings.HasPrefix(arg, "--fleet="):
			cfg.fleet = strings.TrimPrefix(arg, "--fleet=")
			i++
		case strings.HasPrefix(arg, "--token="):
			cfg.token = strings.TrimPrefix(arg, "--token=")
			i++
		default:
			return consoleConfig{}, fmt.Errorf("%w: console does not accept argument %q", ErrUsage, arg)
		}
	}
	return cfg, nil
}

func assignConsoleFlag(cfg *consoleConfig, name, value string) {
	switch name {
	case "addr":
		cfg.addr = value
	case "fleet":
		cfg.fleet = value
	case "token":
		cfg.token = value
	}
}

// openBrowser best-effort launches the operator's browser at url. It never
// fails the command: a headless host (no DISPLAY, no opener) just prints the
// URL the operator already saw. Errors are written to errOut for visibility.
func openBrowser(url string, errOut interface{ Write([]byte) (int, error) }) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		// Linux/BSD: skip when no display is available (headless server).
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(errOut, "console: could not open browser (%v); open the URL above manually\n", err)
		return
	}
	// Reap the child so it does not linger as a zombie; ignore its exit.
	go func() { _ = cmd.Wait() }()
}
