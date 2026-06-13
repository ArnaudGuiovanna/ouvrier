package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

// ErrTrust is the trust command's sentinel, re-exported from internal/deploy
// so errors.Is works across the seam.
var ErrTrust = deploy.ErrTrust

func (app *App) runServerCommand(ctx context.Context, args []string) error {
	if len(args) == 0 || isHelpFlag(args[0]) {
		printServerHelp(app.out)
		if len(args) == 0 {
			return fmt.Errorf("%w: server requires a subcommand (trust)", ErrUsage)
		}
		return nil
	}

	switch args[0] {
	case "trust":
		return app.runServerTrustCommand(ctx, args[1:])
	default:
		return fmt.Errorf("%w: unknown server subcommand %q (expected trust)", ErrUsage, args[0])
	}
}

// trustConfig captures the resolved flag values for `ouvrier server trust`.
type trustConfig struct {
	Host        string // positional <host>; a user@ prefix is tolerated and stripped
	Port        int
	Fingerprint string // expected SHA256 fingerprint for non-interactive (CI) use
	Rotate      bool
	Dir         string // project root holding ouvrier.known_hosts (default ".")
}

func (app *App) runServerTrustCommand(ctx context.Context, args []string) error {
	if hasHelpFlag(args) {
		printServerTrustHelp(app.out)
		return nil
	}
	cfg, err := parseServerTrustFlags(args)
	if err != nil {
		return err
	}

	keyscan := app.keyscan
	if keyscan == nil {
		keyscan = deploy.DefaultKeyscan
	}

	// Scan the host's public keys. The host may carry a user@ prefix the way
	// deploy targets do; keyscan only wants the hostname.
	scanHost := cfg.Host
	if at := strings.LastIndex(scanHost, "@"); at >= 0 {
		scanHost = scanHost[at+1:]
	}
	output, err := keyscan(ctx, scanHost, cfg.Port)
	if err != nil {
		return err
	}
	keys := deploy.ParseKeyscanOutput(output)
	if len(keys) == 0 {
		return fmt.Errorf("%w: ssh-keyscan returned no host keys for %s", ErrTrust, scanHost)
	}

	canonical := deploy.KnownHostsHostname(scanHost, cfg.Port)
	fpKey, _ := deploy.SelectFingerprintKey(keys)
	fingerprint, err := deploy.Fingerprint(fpKey.Key)
	if err != nil {
		return err
	}

	types := make([]string, 0, len(keys))
	for _, k := range keys {
		types = append(types, k.Type)
	}
	fmt.Fprintf(app.out, "scanned %s: %s\n", canonical, strings.Join(types, ", "))
	fmt.Fprintf(app.out, "%s key fingerprint: %s\n", fpKey.Type, fingerprint)

	// Verification: --fingerprint compares non-interactively (CI); otherwise
	// the operator confirms on the terminal. A mismatch or a decline writes
	// nothing and exits nonzero.
	if cfg.Fingerprint != "" {
		expected := deploy.NormalizeFingerprint(cfg.Fingerprint)
		if expected != fingerprint {
			return fmt.Errorf("%w: fingerprint mismatch for %s: expected %s, scanned %s; nothing written", ErrTrust, canonical, expected, fingerprint)
		}
	} else {
		ok, err := app.confirmTrust(canonical)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: aborted; %s was not trusted and nothing was written", ErrTrust, canonical)
		}
	}

	path := filepath.Join(cfg.Dir, deploy.KnownHostsFile)
	result, err := deploy.UpdateKnownHosts(path, canonical, keys, cfg.Rotate)
	if err != nil {
		// Only the changed-key refusal earns the --rotate hint; I/O failures
		// (unwritable file, bad path) must surface untouched — rotating would
		// not fix them.
		if errors.Is(err, deploy.ErrKeyChanged) {
			return fmt.Errorf("%w; to replace the pinned key run `ouvrier server trust --rotate %s`", err, trustHostArgs(scanHost, cfg.Port))
		}
		return err
	}
	switch result {
	case deploy.TrustUnchanged:
		fmt.Fprintf(app.out, "%s is already trusted with the same key(s); nothing to do\n", canonical)
	case deploy.TrustRotated:
		fmt.Fprintf(app.out, "rotated %d host key(s) for %s in %s\n", len(keys), canonical, path)
		fmt.Fprintf(app.out, "commit %s so the whole team and CI trust the new key\n", path)
	default:
		fmt.Fprintf(app.out, "pinned %d host key(s) for %s in %s\n", len(keys), canonical, path)
		fmt.Fprintf(app.out, "commit %s so the whole team and CI trust this host\n", path)
	}
	return nil
}

// confirmTrust asks the operator to confirm the displayed fingerprint on the
// app's input reader. Only an explicit y/yes trusts the host.
func (app *App) confirmTrust(host string) (bool, error) {
	if app.in == nil {
		return false, fmt.Errorf("%w: no interactive input; pass --fingerprint SHA256:... to trust %s non-interactively", ErrTrust, host)
	}
	fmt.Fprintf(app.out, "Trust this host and pin its keys to %s? [y/N]: ", deploy.KnownHostsFile)
	line, err := bufio.NewReader(app.in).ReadString('\n')
	if err != nil && line == "" {
		return false, nil // EOF without an answer = decline
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// trustHostArgs renders the host (plus --port when non-default) for
// copy-pasteable `ouvrier server trust` hints.
func trustHostArgs(host string, port int) string {
	if port != 0 && port != 22 {
		return fmt.Sprintf("%s --port %d", host, port)
	}
	return host
}

func parseServerTrustFlags(args []string) (trustConfig, error) {
	cfg := trustConfig{Dir: "."}
	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "--port":
			value, advance, err := flagValue(args, i, "--port")
			if err != nil {
				return trustConfig{}, err
			}
			port, perr := parsePort(value)
			if perr != nil {
				return trustConfig{}, perr
			}
			cfg.Port = port
			i += advance
		case strings.HasPrefix(arg, "--port="):
			port, perr := parsePort(strings.TrimPrefix(arg, "--port="))
			if perr != nil {
				return trustConfig{}, perr
			}
			cfg.Port = port
			i++
		case arg == "--fingerprint":
			value, advance, err := flagValue(args, i, "--fingerprint")
			if err != nil {
				return trustConfig{}, err
			}
			cfg.Fingerprint = value
			i += advance
		case strings.HasPrefix(arg, "--fingerprint="):
			cfg.Fingerprint = strings.TrimPrefix(arg, "--fingerprint=")
			i++
		case arg == "--dir":
			value, advance, err := flagValue(args, i, "--dir")
			if err != nil {
				return trustConfig{}, err
			}
			cfg.Dir = value
			i += advance
		case strings.HasPrefix(arg, "--dir="):
			cfg.Dir = strings.TrimPrefix(arg, "--dir=")
			i++
		case arg == "--rotate":
			cfg.Rotate = true
			i++
		case strings.HasPrefix(arg, "-"):
			return trustConfig{}, fmt.Errorf("%w: server trust does not accept argument %q", ErrUsage, arg)
		default:
			if cfg.Host != "" {
				return trustConfig{}, fmt.Errorf("%w: server trust accepts exactly one host", ErrUsage)
			}
			cfg.Host = arg
			i++
		}
	}
	if strings.TrimSpace(cfg.Host) == "" {
		return trustConfig{}, fmt.Errorf("%w: server trust requires a host", ErrUsage)
	}
	if cfg.Dir == "" {
		cfg.Dir = "."
	}
	return cfg, nil
}
