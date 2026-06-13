package cli

import (
	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

// parsePipYAML extracts the subset of pip.yaml the show command renders and the
// deploy engine resolves. The parsing lives in internal/deploy (ParsePipYAML) so
// the CLI's `deploy`/`show` commands and the web console resolve environments
// identically; this wrapper adapts the deploy.PipYAML result into the CLI's
// richer pipYAMLSummary (which the show command augments with trigger/model/
// tools/skills/manifest fields parsed elsewhere).
func parsePipYAML(src string) pipYAMLSummary {
	p := deploy.ParsePipYAML(src)
	return pipYAMLSummary{
		Name:       p.Name,
		Version:    p.Version,
		Deploy:     p.Deploy,
		DeployEnvs: p.Envs,
		EnvReq:     p.EnvReq,
		Health:     p.Health,
	}
}
