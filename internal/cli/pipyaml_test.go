package cli

import (
	"reflect"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

func TestParsePipYAMLDeployEnvironments(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want []deploy.Environment
	}{
		{
			name: "flow hosts with options",
			doc: `name: demo
deploy:
  staging:
    hosts: [deploy@stg-1.example.com]
    port: 2222
    path: /opt/ouvrier/demo
    service: ouvrier-demo
    identity: ~/.ssh/ci_ed25519
    sandbox: off
`,
			want: []deploy.Environment{{
				Name:     "staging",
				Hosts:    []string{"deploy@stg-1.example.com"},
				Port:     2222,
				Path:     "/opt/ouvrier/demo",
				Service:  "ouvrier-demo",
				Identity: "~/.ssh/ci_ed25519",
				Sandbox:  "off",
			}},
		},
		{
			name: "multiple hosts including ssh config aliases",
			doc: `deploy:
  prod:
    hosts: [deploy@prod-1, deploy@prod-2, prod-bastion]
`,
			want: []deploy.Environment{{
				Name:  "prod",
				Hosts: []string{"deploy@prod-1", "deploy@prod-2", "prod-bastion"},
			}},
		},
		{
			name: "block list hosts",
			doc: `deploy:
  prod:
    hosts:
      - deploy@prod-1
      - "deploy@prod-2"
`,
			want: []deploy.Environment{{
				Name:  "prod",
				Hosts: []string{"deploy@prod-1", "deploy@prod-2"},
			}},
		},
		{
			name: "multiple environments in document order",
			doc: `deploy:
  staging:
    hosts: [deploy@stg-1]
  prod:
    hosts: [deploy@prod-1, deploy@prod-2]
    port: 22
`,
			want: []deploy.Environment{
				{Name: "staging", Hosts: []string{"deploy@stg-1"}},
				{Name: "prod", Hosts: []string{"deploy@prod-1", "deploy@prod-2"}, Port: 22},
			},
		},
		{
			// Unknown scalar keys and nested mappings under deploy.<env> are
			// ignored, consistent with the parser's tolerance elsewhere.
			name: "unknown keys tolerated like the rest of the parser",
			doc: `deploy:
  staging:
    hosts: [deploy@stg-1]
    flavor: bare-metal
    nested:
      surprise: true
`,
			want: []deploy.Environment{{
				Name:  "staging",
				Hosts: []string{"deploy@stg-1"},
			}},
		},
		{
			name: "legacy ssh and docker targets carry no hosts",
			doc: `deploy:
  ssh:
    host: ops@example.com
    path: /opt/demo
    healthcheck:
      path: /admin/health
  docker:
    image: demo:0.1.0
`,
			want: []deploy.Environment{
				{Name: "ssh", Path: "/opt/demo"},
				{Name: "docker"},
			},
		},
		{
			name: "quoted scalars and inline comments",
			doc: `deploy:
  staging:
    hosts: ["deploy@stg-1", 'deploy@stg-2'] # both quoted
    port: "2200"
    service: 'demo.service'
`,
			want: []deploy.Environment{{
				Name:    "staging",
				Hosts:   []string{"deploy@stg-1", "deploy@stg-2"},
				Port:    2200,
				Service: "demo.service",
			}},
		},
		{
			name: "invalid port degrades to zero",
			doc: `deploy:
  staging:
    hosts: [a@b]
    port: lots
`,
			want: []deploy.Environment{{
				Name:  "staging",
				Hosts: []string{"a@b"},
			}},
		},
		{
			name: "no deploy block",
			doc:  "name: demo\nversion: 0.1.0\n",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePipYAML(tc.doc).DeployEnvs
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DeployEnvs = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParsePipYAMLDeployEnvsDoNotDisturbSummary(t *testing.T) {
	doc := `name: demo
version: 0.3.0
deploy:
  staging:
    hosts: [deploy@stg-1]
  docker:
    image: demo:0.3.0
env:
  required:
    - ANTHROPIC_API_KEY
healthcheck:
  path: /admin/health
`
	got := parsePipYAML(doc)
	if got.Name != "demo" || got.Version != "0.3.0" {
		t.Fatalf("Name/Version = %q/%q", got.Name, got.Version)
	}
	if len(got.Deploy) != 2 || got.Deploy[0] != "staging" || got.Deploy[1] != "docker" {
		t.Fatalf("Deploy = %v, want [staging docker]", got.Deploy)
	}
	if len(got.EnvReq) != 1 || got.EnvReq[0] != "ANTHROPIC_API_KEY" {
		t.Fatalf("EnvReq = %v", got.EnvReq)
	}
	if got.Health != "/admin/health" {
		t.Fatalf("Health = %q", got.Health)
	}
}
