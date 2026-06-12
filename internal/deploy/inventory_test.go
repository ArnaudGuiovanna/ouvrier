package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

func sampleDeployment(name, host string) Deployment {
	return Deployment{
		Name:       name,
		Host:       host,
		User:       "deploy",
		Port:       22,
		Path:       "/opt/ouvrier/" + name,
		Service:    "ouvrier-" + name,
		AdminAddr:  "127.0.0.1:9090",
		HealthPath: "/admin/health",
		SHA256:     "abc123",
		GitRev:     "deadbeef",
		DeployedAt: time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC),
		Result:     "ok",
	}
}

func TestInventoryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployments.json")
	want := sampleDeployment("demo", "prod-1")
	if err := UpsertDeployment(path, want); err != nil {
		t.Fatalf("UpsertDeployment() error = %v", err)
	}

	inv, err := LoadInventory(path)
	if err != nil {
		t.Fatalf("LoadInventory() error = %v", err)
	}
	if inv.Version != 1 {
		t.Fatalf("Version = %d, want 1", inv.Version)
	}
	if len(inv.Deployments) != 1 || inv.Deployments[0] != want {
		t.Fatalf("Deployments = %+v, want [%+v]", inv.Deployments, want)
	}

	// The raw file carries the version marker and is mode 0600.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read inventory: %v", err)
	}
	if !strings.Contains(string(data), `"version": 1`) {
		t.Fatalf("inventory missing version marker:\n%s", data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat inventory: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("inventory mode = %o, want 0600", got)
		}
	}
}

func TestInventoryUpsertReplacesByNameAndHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployments.json")
	first := sampleDeployment("demo", "prod-1")
	if err := UpsertDeployment(path, first); err != nil {
		t.Fatalf("UpsertDeployment() error = %v", err)
	}
	// Same worker on another host: a second entry.
	if err := UpsertDeployment(path, sampleDeployment("demo", "prod-2")); err != nil {
		t.Fatalf("UpsertDeployment() error = %v", err)
	}
	// Redeploy on prod-1: replaces, not appends.
	updated := first
	updated.SHA256 = "def456"
	updated.Result = "rolled-back"
	if err := UpsertDeployment(path, updated); err != nil {
		t.Fatalf("UpsertDeployment() error = %v", err)
	}

	inv, err := LoadInventory(path)
	if err != nil {
		t.Fatalf("LoadInventory() error = %v", err)
	}
	if len(inv.Deployments) != 2 {
		t.Fatalf("Deployments = %+v, want 2 entries", inv.Deployments)
	}
	byHost := map[string]Deployment{}
	for _, d := range inv.Deployments {
		byHost[d.Host] = d
	}
	if byHost["prod-1"].SHA256 != "def456" || byHost["prod-1"].Result != "rolled-back" {
		t.Fatalf("prod-1 entry not replaced: %+v", byHost["prod-1"])
	}
}

func TestInventoryUpsertValidatesKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployments.json")
	if err := UpsertDeployment(path, Deployment{Host: "h"}); !errors.Is(err, ErrDeploy) {
		t.Fatalf("UpsertDeployment(no name) error = %v, want ErrDeploy", err)
	}
	if err := UpsertDeployment(path, Deployment{Name: "n"}); !errors.Is(err, ErrDeploy) {
		t.Fatalf("UpsertDeployment(no host) error = %v, want ErrDeploy", err)
	}
}

func TestInventorySurvivesConcurrentUpserts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployments.json")
	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := sampleDeployment(fmt.Sprintf("worker-%02d", i), "prod-1")
			errs <- UpsertDeployment(path, d)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent UpsertDeployment() error = %v", err)
		}
	}

	inv, err := LoadInventory(path)
	if err != nil {
		t.Fatalf("LoadInventory() after concurrent upserts: %v", err)
	}
	if len(inv.Deployments) != n {
		t.Fatalf("Deployments = %d entries, want %d (lost updates)", len(inv.Deployments), n)
	}
	// The file must always be valid JSON (atomic rename, never a torn write).
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read inventory: %v", err)
	}
	var check Inventory
	if err := json.Unmarshal(data, &check); err != nil {
		t.Fatalf("inventory is not valid JSON after concurrent writes: %v", err)
	}
	// No stray temp files left behind.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

func TestInventoryNeverContainsTokenShapedValues(t *testing.T) {
	t.Setenv(envnames.AdminToken, "sk-admin-super-secret")
	path := filepath.Join(t.TempDir(), "deployments.json")
	if err := UpsertDeployment(path, sampleDeployment("demo", "prod-1")); err != nil {
		t.Fatalf("UpsertDeployment() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read inventory: %v", err)
	}
	for _, forbidden := range []string{"sk-admin-super-secret", "OUVRIER_ADMIN_TOKEN", "Bearer ", "token\":", "ANTHROPIC_API_KEY"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("inventory contains token-shaped content %q:\n%s", forbidden, data)
		}
	}
}

func TestRemoveDeploymentsByNameAndHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployments.json")
	for _, d := range []Deployment{
		sampleDeployment("demo", "prod-1"),
		sampleDeployment("demo", "prod-2"),
		sampleDeployment("other", "prod-1"),
	} {
		if err := UpsertDeployment(path, d); err != nil {
			t.Fatalf("UpsertDeployment() error = %v", err)
		}
	}

	removed, err := RemoveDeployments(path, "demo", "prod-2")
	if err != nil {
		t.Fatalf("RemoveDeployments() error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}

	removed, err = RemoveDeployments(path, "demo", "")
	if err != nil {
		t.Fatalf("RemoveDeployments() error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (only prod-1 left)", removed)
	}

	removed, err = RemoveDeployments(path, "demo", "")
	if err != nil {
		t.Fatalf("RemoveDeployments() error = %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}

	inv, err := LoadInventory(path)
	if err != nil {
		t.Fatalf("LoadInventory() error = %v", err)
	}
	if len(inv.Deployments) != 1 || inv.Deployments[0].Name != "other" {
		t.Fatalf("Deployments = %+v, want only [other]", inv.Deployments)
	}
}

func TestLoadInventoryMissingFileIsEmpty(t *testing.T) {
	inv, err := LoadInventory(filepath.Join(t.TempDir(), "deployments.json"))
	if err != nil {
		t.Fatalf("LoadInventory() error = %v", err)
	}
	if inv.Version != 1 || len(inv.Deployments) != 0 {
		t.Fatalf("LoadInventory() = %+v, want empty version-1 inventory", inv)
	}
}

func TestLoadInventoryRejectsCorruptAndFutureVersions(t *testing.T) {
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "corrupt.json")
	writeFile(t, corrupt, "{not json")
	if _, err := LoadInventory(corrupt); !errors.Is(err, ErrDeploy) {
		t.Fatalf("LoadInventory(corrupt) error = %v, want ErrDeploy", err)
	}

	future := filepath.Join(dir, "future.json")
	writeFile(t, future, `{"version": 99, "deployments": []}`)
	if _, err := LoadInventory(future); !errors.Is(err, ErrDeploy) {
		t.Fatalf("LoadInventory(future version) error = %v, want ErrDeploy", err)
	}
}

func TestInventoryPathPrecedence(t *testing.T) {
	t.Setenv(envnames.FleetPath, "/tmp/custom-fleet.json")
	t.Setenv(envnames.ConfigDir, "/tmp/custom-config")
	got, err := InventoryPath()
	if err != nil {
		t.Fatalf("InventoryPath() error = %v", err)
	}
	if got != "/tmp/custom-fleet.json" {
		t.Fatalf("InventoryPath() = %q, want OUVRIER_FLEET_PATH to win", got)
	}

	t.Setenv(envnames.FleetPath, "")
	got, err = InventoryPath()
	if err != nil {
		t.Fatalf("InventoryPath() error = %v", err)
	}
	if got != filepath.Join("/tmp/custom-config", "deployments.json") {
		t.Fatalf("InventoryPath() = %q, want OUVRIER_CONFIG_DIR/deployments.json", got)
	}

	t.Setenv(envnames.ConfigDir, "")
	got, err = InventoryPath()
	if err != nil {
		t.Fatalf("InventoryPath() error = %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	if got != filepath.Join(home, ".config", "ouvrier", "deployments.json") {
		t.Fatalf("InventoryPath() = %q, want default under ~/.config/ouvrier", got)
	}
}

func TestInventoryEntriesSortedDeterministically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployments.json")
	for _, d := range []Deployment{
		sampleDeployment("zeta", "h1"),
		sampleDeployment("alpha", "h2"),
		sampleDeployment("alpha", "h1"),
	} {
		if err := UpsertDeployment(path, d); err != nil {
			t.Fatalf("UpsertDeployment() error = %v", err)
		}
	}
	inv, err := LoadInventory(path)
	if err != nil {
		t.Fatalf("LoadInventory() error = %v", err)
	}
	gotOrder := make([]string, 0, len(inv.Deployments))
	for _, d := range inv.Deployments {
		gotOrder = append(gotOrder, d.Name+"/"+d.Host)
	}
	want := []string{"alpha/h1", "alpha/h2", "zeta/h1"}
	for i := range want {
		if gotOrder[i] != want[i] {
			t.Fatalf("order = %v, want %v", gotOrder, want)
		}
	}
}
