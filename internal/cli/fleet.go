package cli

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

func (app *App) runFleetCommand(ctx context.Context, args []string) error {
	if len(args) == 0 || isHelpFlag(args[0]) {
		printFleetHelp(app.out)
		if len(args) == 0 {
			return fmt.Errorf("%w: fleet requires a subcommand (ls|rm)", ErrUsage)
		}
		return nil
	}

	switch args[0] {
	case "ls":
		return app.runFleetLsCommand(ctx, args[1:])
	case "rm":
		return app.runFleetRmCommand(ctx, args[1:])
	default:
		return fmt.Errorf("%w: unknown fleet subcommand %q (expected ls|rm)", ErrUsage, args[0])
	}
}

func (app *App) runFleetLsCommand(_ context.Context, args []string) error {
	if hasHelpFlag(args) {
		printFleetLsHelp(app.out)
		return nil
	}
	if len(args) > 0 {
		return fmt.Errorf("%w: fleet ls does not accept arguments", ErrUsage)
	}

	path, err := deploy.InventoryPath()
	if err != nil {
		return err
	}
	inv, err := deploy.LoadInventory(path)
	if err != nil {
		return err
	}
	if len(inv.Deployments) == 0 {
		fmt.Fprintf(app.out, "no deployments recorded (%s)\n", path)
		return nil
	}

	w := tabwriter.NewWriter(app.out, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tHOST\tSERVICE\tDEPLOYED\tRESULT")
	for _, d := range inv.Deployments {
		deployed := ""
		if !d.DeployedAt.IsZero() {
			deployed = d.DeployedAt.UTC().Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", d.Name, fleetHostColumn(d), d.Service, deployed, d.Result)
	}
	return w.Flush()
}

// fleetHostColumn renders user@host[:port] the way an operator would ssh it.
func fleetHostColumn(d deploy.Deployment) string {
	host := d.Host
	if d.User != "" {
		host = d.User + "@" + host
	}
	if d.Port != 0 {
		host = fmt.Sprintf("%s:%d", host, d.Port)
	}
	return host
}

func (app *App) runFleetRmCommand(_ context.Context, args []string) error {
	if hasHelpFlag(args) {
		printFleetRmHelp(app.out)
		return nil
	}

	name := ""
	host := ""
	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "--host":
			value, advance, err := flagValue(args, i, "--host")
			if err != nil {
				return err
			}
			host = value
			i += advance
		case strings.HasPrefix(arg, "--host="):
			host = strings.TrimPrefix(arg, "--host=")
			i++
		case strings.HasPrefix(arg, "-"):
			return fmt.Errorf("%w: fleet rm does not accept argument %q", ErrUsage, arg)
		default:
			if name != "" {
				return fmt.Errorf("%w: fleet rm accepts exactly one worker name", ErrUsage)
			}
			name = arg
			i++
		}
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: fleet rm requires a worker name", ErrUsage)
	}

	path, err := deploy.InventoryPath()
	if err != nil {
		return err
	}
	removed, err := deploy.RemoveDeployments(path, name, host)
	if err != nil {
		return err
	}
	if removed == 0 {
		if host != "" {
			return fmt.Errorf("%w: no recorded deployment of %q on host %q", ErrDeploy, name, host)
		}
		return fmt.Errorf("%w: no recorded deployment named %q", ErrDeploy, name)
	}
	fmt.Fprintf(app.out, "removed %d deployment(s) from %s\n", removed, path)
	return nil
}
