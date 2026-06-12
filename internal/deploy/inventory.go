package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

// inventoryVersion is the on-disk schema version of deployments.json.
const inventoryVersion = 1

// Deployment is one recorded worker deployment. It is a disposable cache for
// `ouvrier console`/`fleet` — live /admin/health is truth. No secrets ever:
// there is deliberately no field that could hold a token or env value.
type Deployment struct {
	Name       string    `json:"name"`
	Host       string    `json:"host"`
	User       string    `json:"user,omitempty"`
	Port       int       `json:"port,omitempty"`
	Path       string    `json:"path,omitempty"`
	Service    string    `json:"service,omitempty"`
	AdminAddr  string    `json:"admin_addr,omitempty"`
	HealthPath string    `json:"health_path,omitempty"`
	SHA256     string    `json:"sha256,omitempty"`
	GitRev     string    `json:"git_rev,omitempty"`
	DeployedAt time.Time `json:"deployed_at"`
	Result     string    `json:"result,omitempty"`
}

// Inventory is the user-level deployments file at
// ~/.config/ouvrier/deployments.json.
type Inventory struct {
	Version     int          `json:"version"`
	Deployments []Deployment `json:"deployments"`
}

// InventoryPath resolves the deployments.json location. OUVRIER_FLEET_PATH
// overrides the full path; OUVRIER_CONFIG_DIR overrides the config directory;
// the default is ~/.config/ouvrier/deployments.json.
func InventoryPath() (string, error) {
	if path := strings.TrimSpace(os.Getenv(envnames.FleetPath)); path != "" {
		return path, nil
	}
	if dir := strings.TrimSpace(os.Getenv(envnames.ConfigDir)); dir != "" {
		return filepath.Join(dir, "deployments.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("%w: resolve home directory for deployments inventory: %w", ErrDeploy, err)
	}
	return filepath.Join(home, ".config", "ouvrier", "deployments.json"), nil
}

// LoadInventory reads the inventory at path. A missing file yields an empty
// version-1 inventory; a corrupt or future-versioned file is an error.
func LoadInventory(path string) (Inventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Inventory{Version: inventoryVersion}, nil
		}
		return Inventory{}, fmt.Errorf("%w: read deployments inventory %s: %w", ErrDeploy, path, err)
	}
	var inv Inventory
	if err := json.Unmarshal(data, &inv); err != nil {
		return Inventory{}, fmt.Errorf("%w: parse deployments inventory %s: %w", ErrDeploy, path, err)
	}
	if inv.Version == 0 {
		inv.Version = inventoryVersion
	}
	if inv.Version != inventoryVersion {
		return Inventory{}, fmt.Errorf("%w: deployments inventory %s has unsupported version %d (this build supports %d)", ErrDeploy, path, inv.Version, inventoryVersion)
	}
	return inv, nil
}

// UpsertDeployment inserts or replaces the inventory entry keyed by
// (name, host). The read-modify-write cycle runs under an exclusive lock on a
// sidecar lock file and lands via an atomic tmp+rename, so concurrent deploys
// never corrupt or drop each other's entries.
func UpsertDeployment(path string, d Deployment) error {
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("%w: deployment record requires a name", ErrDeploy)
	}
	if strings.TrimSpace(d.Host) == "" {
		return fmt.Errorf("%w: deployment record requires a host", ErrDeploy)
	}

	return withInventoryLock(path, func() error {
		inv, err := LoadInventory(path)
		if err != nil {
			return err
		}
		replaced := false
		for i := range inv.Deployments {
			if inv.Deployments[i].Name == d.Name && inv.Deployments[i].Host == d.Host {
				inv.Deployments[i] = d
				replaced = true
				break
			}
		}
		if !replaced {
			inv.Deployments = append(inv.Deployments, d)
		}
		return writeInventory(path, inv)
	})
}

// RemoveDeployments deletes every entry matching name, narrowed to one host
// when host is non-empty. It returns how many entries were removed.
func RemoveDeployments(path, name, host string) (int, error) {
	if strings.TrimSpace(name) == "" {
		return 0, fmt.Errorf("%w: deployment name is required", ErrDeploy)
	}

	removed := 0
	err := withInventoryLock(path, func() error {
		inv, err := LoadInventory(path)
		if err != nil {
			return err
		}
		kept := inv.Deployments[:0]
		for _, d := range inv.Deployments {
			if d.Name == name && (host == "" || d.Host == host) {
				removed++
				continue
			}
			kept = append(kept, d)
		}
		if removed == 0 {
			return nil
		}
		inv.Deployments = kept
		return writeInventory(path, inv)
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// withInventoryLock serializes inventory mutations across processes via an
// exclusive flock on a sidecar lock file next to the inventory.
func withInventoryLock(path string, fn func() error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%w: create inventory directory %s: %w", ErrDeploy, dir, err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("%w: open inventory lock: %w", ErrDeploy, err)
	}
	defer lock.Close()
	if err := flockExclusive(lock); err != nil {
		return fmt.Errorf("%w: lock inventory: %w", ErrDeploy, err)
	}
	defer flockRelease(lock)
	return fn()
}

// writeInventory marshals inv with the stdlib encoder and lands it atomically
// (temp file in the same directory, 0600, then rename).
func writeInventory(path string, inv Inventory) error {
	inv.Version = inventoryVersion
	sort.SliceStable(inv.Deployments, func(i, j int) bool {
		if inv.Deployments[i].Name != inv.Deployments[j].Name {
			return inv.Deployments[i].Name < inv.Deployments[j].Name
		}
		return inv.Deployments[i].Host < inv.Deployments[j].Host
	})

	data, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: encode deployments inventory: %w", ErrDeploy, err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".deployments-*.tmp")
	if err != nil {
		return fmt.Errorf("%w: create inventory temp file: %w", ErrDeploy, err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("%w: chmod inventory temp file: %w", ErrDeploy, err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("%w: write deployments inventory: %w", ErrDeploy, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("%w: close inventory temp file: %w", ErrDeploy, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("%w: replace deployments inventory: %w", ErrDeploy, err)
	}
	return nil
}
