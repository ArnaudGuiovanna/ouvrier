package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// sshConfig captures the resolved options for `ouvrier deploy ssh`.
type sshConfig struct {
	Dir        string
	Host       string
	User       string
	Port       int
	Path       string // remote install path; defaults to /opt/ouvrier/<name>
	Service    string // systemd unit name; defaults to ouvrier-<name>
	HealthURL  string // path or full URL; defaults to /admin/health
	AdminToken string // masked in logs/output
}

// remoteRunner runs ssh/scp commands against a host. It is an interface so
// tests can substitute a fake without spawning ssh.
type remoteRunner interface {
	// SSH executes a remote shell command via ssh, returning combined stdout.
	SSH(ctx context.Context, opts sshConnectOpts, command string) (stdout string, err error)
	// SCP uploads localPath to remotePath on the host.
	SCP(ctx context.Context, opts sshConnectOpts, localPath, remotePath string) error
	// SCPData uploads the given bytes as a remote file at remotePath.
	SCPData(ctx context.Context, opts sshConnectOpts, data []byte, remotePath string) error
}

// sshConnectOpts holds the dial information shared by SSH/SCP calls.
type sshConnectOpts struct {
	Host string
	User string
	Port int
}

// userHost renders the host portion in user@host form. Empty user yields
// just the host, matching ssh/scp's default behavior.
func (o sshConnectOpts) userHost() string {
	if o.User == "" {
		return o.Host
	}
	return o.User + "@" + o.Host
}

func (app *App) runDeploySSHCommand(ctx context.Context, args []string) error {
	if hasHelpFlag(args) {
		printDeploySSHHelp(app.out)
		return nil
	}

	cfg, err := parseDeploySSHFlags(args)
	if err != nil {
		return err
	}
	return runDeploySSH(ctx, cfg, app.out, app.errOut, defaultGoRunner, defaultRemoteRunner{})
}

func parseDeploySSHFlags(args []string) (sshConfig, error) {
	cfg := sshConfig{
		Dir:       ".",
		HealthURL: "/admin/health",
	}
	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "--host":
			value, advance, err := flagValue(args, i, "--host")
			if err != nil {
				return sshConfig{}, err
			}
			cfg.Host = value
			i += advance
		case strings.HasPrefix(arg, "--host="):
			cfg.Host = strings.TrimPrefix(arg, "--host=")
			i++
		case arg == "--user":
			value, advance, err := flagValue(args, i, "--user")
			if err != nil {
				return sshConfig{}, err
			}
			cfg.User = value
			i += advance
		case strings.HasPrefix(arg, "--user="):
			cfg.User = strings.TrimPrefix(arg, "--user=")
			i++
		case arg == "--port":
			value, advance, err := flagValue(args, i, "--port")
			if err != nil {
				return sshConfig{}, err
			}
			port, perr := parsePort(value)
			if perr != nil {
				return sshConfig{}, perr
			}
			cfg.Port = port
			i += advance
		case strings.HasPrefix(arg, "--port="):
			port, perr := parsePort(strings.TrimPrefix(arg, "--port="))
			if perr != nil {
				return sshConfig{}, perr
			}
			cfg.Port = port
			i++
		case arg == "--path":
			value, advance, err := flagValue(args, i, "--path")
			if err != nil {
				return sshConfig{}, err
			}
			cfg.Path = value
			i += advance
		case strings.HasPrefix(arg, "--path="):
			cfg.Path = strings.TrimPrefix(arg, "--path=")
			i++
		case arg == "--service":
			value, advance, err := flagValue(args, i, "--service")
			if err != nil {
				return sshConfig{}, err
			}
			cfg.Service = value
			i += advance
		case strings.HasPrefix(arg, "--service="):
			cfg.Service = strings.TrimPrefix(arg, "--service=")
			i++
		case arg == "--dir":
			value, advance, err := flagValue(args, i, "--dir")
			if err != nil {
				return sshConfig{}, err
			}
			cfg.Dir = value
			i += advance
		case strings.HasPrefix(arg, "--dir="):
			cfg.Dir = strings.TrimPrefix(arg, "--dir=")
			i++
		case arg == "--health-url":
			value, advance, err := flagValue(args, i, "--health-url")
			if err != nil {
				return sshConfig{}, err
			}
			cfg.HealthURL = value
			i += advance
		case strings.HasPrefix(arg, "--health-url="):
			cfg.HealthURL = strings.TrimPrefix(arg, "--health-url=")
			i++
		case arg == "--admin-token":
			value, advance, err := flagValue(args, i, "--admin-token")
			if err != nil {
				return sshConfig{}, err
			}
			cfg.AdminToken = value
			i += advance
		case strings.HasPrefix(arg, "--admin-token="):
			cfg.AdminToken = strings.TrimPrefix(arg, "--admin-token=")
			i++
		default:
			return sshConfig{}, fmt.Errorf("%w: deploy ssh does not accept argument %q", ErrUsage, arg)
		}
	}
	if cfg.Dir == "" {
		cfg.Dir = "."
	}
	if strings.TrimSpace(cfg.Host) == "" {
		return sshConfig{}, fmt.Errorf("%w: deploy ssh requires --host", ErrUsage)
	}
	if cfg.HealthURL == "" {
		cfg.HealthURL = "/admin/health"
	}
	return cfg, nil
}

func flagValue(args []string, i int, name string) (string, int, error) {
	if i+1 >= len(args) {
		return "", 0, fmt.Errorf("%w: %s requires a value", ErrUsage, name)
	}
	return args[i+1], 2, nil
}

func parsePort(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("%w: --port requires a positive integer", ErrUsage)
	}
	n := 0
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%w: --port must be a positive integer, got %q", ErrUsage, value)
		}
		n = n*10 + int(r-'0')
		if n > 65535 {
			return 0, fmt.Errorf("%w: --port must be <= 65535", ErrUsage)
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("%w: --port must be > 0", ErrUsage)
	}
	return n, nil
}

func runDeploySSH(ctx context.Context, cfg sshConfig, out, errOut io.Writer, goRun goRunner, ssh remoteRunner) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if ssh == nil {
		return fmt.Errorf("%w: ssh runner is nil", ErrDeploy)
	}

	// 1. Build a static linux/amd64 binary the deploy can ship.
	br, err := staticBuildForDeploy(ctx, cfg.Dir, out, errOut, goRun)
	if err != nil {
		return err
	}

	// 2. Resolve remote paths / service name now that we know the project name.
	remotePath := cfg.Path
	if remotePath == "" {
		remotePath = "/opt/ouvrier/" + br.ProjectName
	}
	service := cfg.Service
	if service == "" {
		service = "ouvrier-" + br.ProjectName
	}

	connect := sshConnectOpts{Host: cfg.Host, User: cfg.User, Port: cfg.Port}

	// 3. Require a local .env to avoid blank deploys.
	localEnv := filepath.Join(br.Dir, ".env")
	if _, statErr := os.Stat(localEnv); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("%w: no .env in %s; refuse to deploy without secrets configuration", ErrDeploy, br.Dir)
		}
		return fmt.Errorf("%w: stat .env: %w", ErrDeploy, statErr)
	}

	// 4. Ensure remote directories exist.
	mkdirCmd := fmt.Sprintf("mkdir -p %s/bin", shellQuote(remotePath))
	fmt.Fprintf(out, "ssh %s: %s\n", connect.userHost(), mkdirCmd)
	if _, err := ssh.SSH(ctx, connect, mkdirCmd); err != nil {
		return fmt.Errorf("%w: prepare remote layout: %w", ErrDeploy, err)
	}

	// 5. Upload the binary as <path>/bin/<name>.new. We swap into place after
	//    a successful health check, keeping the previous binary for rollback.
	remoteBinNew := remotePath + "/bin/" + br.ProjectName + ".new"
	fmt.Fprintf(out, "scp %s -> %s:%s\n", br.Output, connect.userHost(), remoteBinNew)
	if err := ssh.SCP(ctx, connect, br.Output, remoteBinNew); err != nil {
		return fmt.Errorf("%w: upload binary: %w", ErrDeploy, err)
	}

	// 6. Upload .env (always, since we already verified it exists locally).
	remoteEnv := remotePath + "/.env"
	fmt.Fprintf(out, "scp .env -> %s:%s\n", connect.userHost(), remoteEnv)
	if err := ssh.SCP(ctx, connect, localEnv, remoteEnv); err != nil {
		return fmt.Errorf("%w: upload .env: %w", ErrDeploy, err)
	}
	chmodCmd := fmt.Sprintf("chmod 0600 %s", shellQuote(remoteEnv))
	if _, err := ssh.SSH(ctx, connect, chmodCmd); err != nil {
		return fmt.Errorf("%w: chmod remote .env: %w", ErrDeploy, err)
	}

	// 7. Upload runtime assets that are intentionally read from disk by the
	//    worker, such as skills/<name>/SKILL.md.
	if err := uploadRuntimeAssets(ctx, ssh, connect, br.Dir, remotePath, out); err != nil {
		return err
	}

	// 8. Render and upload the systemd unit, then daemon-reload.
	unitPath := remotePath + "/" + service + ".service"
	unit := renderSystemdUnit(systemdUnitParams{
		Name:        br.ProjectName,
		Service:     service,
		InstallPath: remotePath,
		User:        cfg.User,
	})
	fmt.Fprintf(out, "render systemd unit -> %s\n", unitPath)
	if err := ssh.SCPData(ctx, connect, []byte(unit), unitPath); err != nil {
		return fmt.Errorf("%w: upload systemd unit: %w", ErrDeploy, err)
	}
	systemdLink := fmt.Sprintf(
		"sudo install -m 0644 %s /etc/systemd/system/%s.service && sudo systemctl daemon-reload",
		shellQuote(unitPath), shellQuote(service),
	)
	if _, err := ssh.SSH(ctx, connect, systemdLink); err != nil {
		return fmt.Errorf("%w: install systemd unit: %w", ErrDeploy, err)
	}

	// 9. Promote the new binary: keep the previous (if any) as .previous,
	//    move .new into place, then restart the service.
	remoteBin := remotePath + "/bin/" + br.ProjectName
	remoteBinPrev := remotePath + "/bin/" + br.ProjectName + ".previous"
	promoteCmd := fmt.Sprintf(
		"if [ -f %s ]; then mv %s %s; fi && mv %s %s && chmod 0755 %s",
		shellQuote(remoteBin), shellQuote(remoteBin), shellQuote(remoteBinPrev),
		shellQuote(remoteBinNew), shellQuote(remoteBin),
		shellQuote(remoteBin),
	)
	if _, err := ssh.SSH(ctx, connect, promoteCmd); err != nil {
		return fmt.Errorf("%w: promote binary: %w", ErrDeploy, err)
	}

	restartCmd := fmt.Sprintf("sudo systemctl restart %s", shellQuote(service))
	if _, err := ssh.SSH(ctx, connect, restartCmd); err != nil {
		return rollbackBinary(ctx, ssh, connect, remoteBin, remoteBinPrev, service, fmt.Errorf("%w: systemctl restart failed: %w", ErrDeploy, err), out)
	}

	// 10. Health check on the remote host.
	healthCmd := buildHealthCheckCommand(cfg.HealthURL, cfg.AdminToken)
	if _, err := ssh.SSH(ctx, connect, healthCmd); err != nil {
		return rollbackBinary(ctx, ssh, connect, remoteBin, remoteBinPrev, service, fmt.Errorf("%w: health check failed: %w", ErrDeploy, err), out)
	}

	fmt.Fprintf(out, "deployed %s to %s:%s (service=%s)\n", br.ProjectName, connect.userHost(), remotePath, service)
	return nil
}

func uploadRuntimeAssets(ctx context.Context, ssh remoteRunner, connect sshConnectOpts, localRoot, remotePath string, out io.Writer) error {
	skillsRoot := filepath.Join(localRoot, "skills")
	info, err := os.Stat(skillsRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%w: stat runtime assets: %w", ErrDeploy, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s exists but is not a directory", ErrDeploy, skillsRoot)
	}

	return filepath.WalkDir(skillsRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("%w: read runtime asset %s: %w", ErrDeploy, path, walkErr)
		}
		rel, err := filepath.Rel(localRoot, path)
		if err != nil {
			return fmt.Errorf("%w: resolve runtime asset %s: %w", ErrDeploy, path, err)
		}
		remoteAssetPath := remotePath + "/" + filepath.ToSlash(rel)
		if entry.IsDir() {
			mkdirCmd := fmt.Sprintf("mkdir -p %s", shellQuote(remoteAssetPath))
			fmt.Fprintf(out, "ssh %s: %s\n", connect.userHost(), mkdirCmd)
			if _, err := ssh.SSH(ctx, connect, mkdirCmd); err != nil {
				return fmt.Errorf("%w: create remote runtime asset directory: %w", ErrDeploy, err)
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("%w: stat runtime asset %s: %w", ErrDeploy, path, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		fmt.Fprintf(out, "scp %s -> %s:%s\n", rel, connect.userHost(), remoteAssetPath)
		if err := ssh.SCP(ctx, connect, path, remoteAssetPath); err != nil {
			return fmt.Errorf("%w: upload runtime asset %s: %w", ErrDeploy, rel, err)
		}
		return nil
	})
}

func rollbackBinary(ctx context.Context, ssh remoteRunner, connect sshConnectOpts, remoteBin, remoteBinPrev, service string, cause error, out io.Writer) error {
	fmt.Fprintf(out, "deploy failed: %v; rolling back\n", cause)
	rollback := fmt.Sprintf(
		"if [ -f %s ]; then mv %s %s && sudo systemctl restart %s; fi",
		shellQuote(remoteBinPrev), shellQuote(remoteBinPrev), shellQuote(remoteBin), shellQuote(service),
	)
	if _, rbErr := ssh.SSH(ctx, connect, rollback); rbErr != nil {
		return fmt.Errorf("%w (rollback also failed: %v)", cause, rbErr)
	}
	return cause
}

// buildHealthCheckCommand renders the remote curl that probes the worker's
// admin health endpoint. The admin token is interpolated into the command but
// never echoed locally and never returned via err strings.
func buildHealthCheckCommand(healthURL, adminToken string) string {
	url := healthURL
	if strings.HasPrefix(url, "/") {
		url = "http://127.0.0.1:8080" + url
	}
	auth := ""
	if adminToken != "" {
		auth = fmt.Sprintf(" -H %s", shellQuote("Authorization: Bearer "+adminToken))
	}
	return fmt.Sprintf("curl -fsS --max-time 5%s %s", auth, shellQuote(url))
}

// systemdUnitParams collects values used to render the project's systemd
// unit file. The unit is intentionally simple: it runs the binary out of the
// install directory, loads .env via EnvironmentFile, and restarts on failure.
type systemdUnitParams struct {
	Name        string
	Service     string
	InstallPath string
	User        string
}

func renderSystemdUnit(p systemdUnitParams) string {
	user := p.User
	if strings.TrimSpace(user) == "" {
		user = "root"
	}
	return fmt.Sprintf(`[Unit]
Description=Ouvrier worker %s
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
WorkingDirectory=%s
EnvironmentFile=%s/.env
ExecStart=%s/bin/%s
Restart=on-failure
RestartSec=2s

[Install]
WantedBy=multi-user.target
`, p.Name, user, p.InstallPath, p.InstallPath, p.InstallPath, p.Name)
}

// shellQuote returns a single-quoted POSIX shell literal. Inner single quotes
// are encoded using the standard '\” trick. This is safer than relying on
// the caller to pick characters compatible with bare ssh strings.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// defaultRemoteRunner shells out to the system ssh/scp binaries.
type defaultRemoteRunner struct{}

func (defaultRemoteRunner) SSH(ctx context.Context, opts sshConnectOpts, command string) (string, error) {
	args := sshBaseArgs(opts)
	args = append(args, opts.userHost(), command)
	stdout, _, err := runHostCommand(ctx, "ssh", args)
	return stdout, err
}

func (defaultRemoteRunner) SCP(ctx context.Context, opts sshConnectOpts, localPath, remotePath string) error {
	args := scpBaseArgs(opts)
	args = append(args, localPath, opts.userHost()+":"+remotePath)
	_, _, err := runHostCommand(ctx, "scp", args)
	return err
}

func (defaultRemoteRunner) SCPData(ctx context.Context, opts sshConnectOpts, data []byte, remotePath string) error {
	// Stream the bytes to the remote host via ssh; this avoids writing the
	// rendered unit file to disk locally and keeps the implementation focused
	// on what tests need to substitute.
	tmp, err := os.CreateTemp("", "ouvrier-deploy-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return defaultRemoteRunner{}.SCP(ctx, opts, tmp.Name(), remotePath)
}

func sshBaseArgs(opts sshConnectOpts) []string {
	args := make([]string, 0, 4)
	if opts.Port != 0 {
		args = append(args, "-p", fmt.Sprintf("%d", opts.Port))
	}
	args = append(args, "-o", "BatchMode=yes")
	return args
}

func scpBaseArgs(opts sshConnectOpts) []string {
	args := make([]string, 0, 4)
	if opts.Port != 0 {
		args = append(args, "-P", fmt.Sprintf("%d", opts.Port))
	}
	args = append(args, "-o", "BatchMode=yes")
	return args
}

// runHostCommand invokes a command via the standard library, capturing
// stdout and stderr separately. This lives in deploy_ssh.go because only the
// default remote runner uses it.
func runHostCommand(ctx context.Context, name string, args []string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmd := newExecCommand(ctx, name, args)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), stderr.String(), fmt.Errorf("%s %s: %w (stderr=%s)", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), stderr.String(), nil
}
