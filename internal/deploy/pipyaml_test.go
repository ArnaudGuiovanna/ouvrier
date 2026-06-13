package deploy

import "testing"

func TestParsePipYAMLExtractsEnvironments(t *testing.T) {
	src := `name: demo
version: 1.2.3
healthcheck:
  path: /admin/health
env:
  required:
    - OUVRIER_ADMIN_TOKEN
    - DATABASE_URL
deploy:
  staging:
    hosts: [deploy@stg1, deploy@stg2]
    port: 2222
    path: /opt/demo
    service: ouvrier-demo
    sandbox: "off"
  prod:
    hosts:
      - deploy@prod1
`
	p := ParsePipYAML(src)
	if p.Name != "demo" || p.Version != "1.2.3" {
		t.Fatalf("name/version = %q/%q", p.Name, p.Version)
	}
	if p.Health != "/admin/health" {
		t.Fatalf("health = %q", p.Health)
	}
	if len(p.EnvReq) != 2 || p.EnvReq[0] != "OUVRIER_ADMIN_TOKEN" {
		t.Fatalf("env required = %v", p.EnvReq)
	}
	stg := p.DeployEnv("staging")
	if stg == nil {
		t.Fatal("staging env not parsed")
	}
	if len(stg.Hosts) != 2 || stg.Hosts[0] != "deploy@stg1" {
		t.Fatalf("staging hosts = %v", stg.Hosts)
	}
	if stg.Port != 2222 || stg.Path != "/opt/demo" || stg.Service != "ouvrier-demo" || stg.Sandbox != "off" {
		t.Fatalf("staging scalars = %+v", stg)
	}
	prod := p.DeployEnv("prod")
	if prod == nil || len(prod.Hosts) != 1 || prod.Hosts[0] != "deploy@prod1" {
		t.Fatalf("prod env = %+v", prod)
	}

	// ResolveEnvironment integrates with the parsed result.
	env, err := ResolveEnvironment(p.Envs, "staging")
	if err != nil {
		t.Fatalf("resolve staging: %v", err)
	}
	if env.Port != 2222 {
		t.Fatalf("resolved port = %d", env.Port)
	}
}
