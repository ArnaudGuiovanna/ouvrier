# Migration `PIP_*` → `OUVRIER_*` — Plan d'implémentation

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Renommer les 4 variables d'environnement legacy (`PIP_ENV`, `PIP_ADMIN_TOKEN`, `PIP_ADDR`, `PIP_LOG_LEVEL`) en `OUVRIER_*`, sans rétrocompatibilité, avec constantes centralisées et garde-fou fail-fast au démarrage.

**Architecture:** Un nouveau package `internal/envnames` est l'unique source de vérité pour les noms (nouveaux et legacy). Le runtime, la CLI et le scaffold consomment ses constantes. Une fonction `checkLegacyEnv()` (racine, `env_guard.go`) refuse le démarrage de `Run()` et `Runner.Handler()` si un ancien nom exact est encore défini. Spec : `docs/superpowers/specs/2026-06-11-env-migration-design.md`.

**Tech Stack:** Go 1.25, stdlib uniquement. Gates CI : `gofmt`, `go vet`, `staticcheck`, `go test ./...`.

**Ordre imposé :** la migration des lectures runtime + tests (Task 2) précède le câblage du garde-fou (Task 4), sinon les tests existants qui posent `PIP_ENV=dev` casseraient au commit intermédiaire.

---

### Task 1: Package `internal/envnames`

**Files:**
- Create: `internal/envnames/envnames.go`

- [ ] **Step 1: Créer le package**

```go
// Package envnames is the single source of truth for the runtime environment
// variable names read by Ouvrier. Legacy PIP_* names are listed only so the
// startup guard can fail loudly when one is still set; they are never read.
package envnames

const (
	Env        = "OUVRIER_ENV"
	AdminToken = "OUVRIER_ADMIN_TOKEN"
	Addr       = "OUVRIER_ADDR"
	LogLevel   = "OUVRIER_LOG_LEVEL"

	LegacyEnv        = "PIP_ENV"
	LegacyAdminToken = "PIP_ADMIN_TOKEN"
	LegacyAddr       = "PIP_ADDR"
	LegacyLogLevel   = "PIP_LOG_LEVEL"
)

// Legacy maps each retired name to its replacement, for the startup guard
// and its error messages.
var Legacy = map[string]string{
	LegacyEnv:        Env,
	LegacyAdminToken: AdminToken,
	LegacyAddr:       Addr,
	LegacyLogLevel:   LogLevel,
}
```

- [ ] **Step 2: Vérifier la compilation**

Run: `cd /home/ubuntu/ouvrier && go build ./...`
Expected: succès, aucune sortie.

- [ ] **Step 3: Commit**

```bash
git add internal/envnames/envnames.go
git commit -m "feat: add internal/envnames as single source of env var names"
```

---

### Task 2: Migrer les lectures runtime + tests racine (le rename effectif)

**Files:**
- Modify: `provider_env.go:116-118` (`adminTokenFromEnv`)
- Modify: `routes.go:1114-1116` (`adminDevModeEnabled`), `routes.go:1169` et `routes.go:1179` (messages d'erreur/warning)
- Modify: tous les `*_test.go` à la racine qui utilisent `PIP_*` (sed mécanique)

- [ ] **Step 1: Migrer `adminTokenFromEnv` dans `provider_env.go`**

Ajouter l'import `"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"` puis :

```go
func adminTokenFromEnv() string {
	return strings.TrimSpace(os.Getenv(envnames.AdminToken))
}
```

- [ ] **Step 2: Migrer `routes.go`**

Ajouter l'import `"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"`. Remplacer `adminDevModeEnabled` :

```go
func adminDevModeEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(envnames.Env)), "dev")
}
```

Dans `checkAdminExposure` (ligne ~1169), remplacer le `return fmt.Errorf(...)` par :

```go
	return fmt.Errorf("refusing to start: admin endpoints are unauthenticated (%s=dev, no %s) and %q is reachable from the network; set %s for production, bind to localhost for local dev, or set %s=1 to override", envnames.Env, envnames.AdminToken, addr, envnames.AdminToken, adminInsecureEnv)
```

Dans `adminExposureWarning` (ligne ~1179), remplacer le `return fmt.Sprintf(...)` par :

```go
	return fmt.Sprintf("WARNING: admin endpoints are UNAUTHENTICATED (%s=dev, no %s) on %q", envnames.Env, envnames.AdminToken, addr)
```

- [ ] **Step 3: Vérifier que les tests racine échouent (rouge attendu)**

Run: `go test . 2>&1 | tail -5`
Expected: FAIL — les tests posent encore `t.Setenv("PIP_ENV", "dev")` alors que le runtime lit `OUVRIER_ENV`.

- [ ] **Step 4: Migrer les tests racine (sed mécanique)**

```bash
cd /home/ubuntu/ouvrier && sed -i 's/PIP_ENV/OUVRIER_ENV/g; s/PIP_ADMIN_TOKEN/OUVRIER_ADMIN_TOKEN/g; s/PIP_ADDR/OUVRIER_ADDR/g; s/PIP_LOG_LEVEL/OUVRIER_LOG_LEVEL/g' ./*_test.go
```

(Cible uniquement la racine : les tests de `internal/cli` et `internal/scaffold` migrent dans les Tasks 5 et 6. Le sed couvre aussi les assertions sur les messages d'erreur de `admin_exposure_test.go`, qui correspondent désormais aux nouveaux textes.)

- [ ] **Step 5: Vérifier le vert**

Run: `go test . 2>&1 | tail -3`
Expected: `ok  github.com/ArnaudGuiovanna/ouvrier`

- [ ] **Step 6: Commit**

```bash
git add provider_env.go routes.go ./*_test.go
git commit -m "feat!: rename PIP_* env vars to OUVRIER_*

BREAKING CHANGE: PIP_ENV, PIP_ADMIN_TOKEN, PIP_ADDR and PIP_LOG_LEVEL are
no longer read. Use OUVRIER_ENV, OUVRIER_ADMIN_TOKEN, OUVRIER_ADDR and
OUVRIER_LOG_LEVEL."
```

---

### Task 3: Garde-fou `checkLegacyEnv()`

**Files:**
- Create: `env_guard.go`
- Test: `env_guard_test.go`

- [ ] **Step 1: Écrire les tests (rouges)**

`env_guard_test.go` :

```go
package ovr

import (
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

func TestCheckLegacyEnvAllowsCleanEnv(t *testing.T) {
	for legacy := range envnames.Legacy {
		t.Setenv(legacy, "")
	}
	t.Setenv(envnames.Env, "dev")
	if err := checkLegacyEnv(); err != nil {
		t.Fatalf("expected nil with only OUVRIER_* set, got %v", err)
	}
}

func TestCheckLegacyEnvRejectsEachLegacyName(t *testing.T) {
	for legacy, replacement := range envnames.Legacy {
		t.Run(legacy, func(t *testing.T) {
			t.Setenv(legacy, "value")
			err := checkLegacyEnv()
			if err == nil {
				t.Fatalf("expected error when %s is set", legacy)
			}
			if !strings.Contains(err.Error(), legacy) || !strings.Contains(err.Error(), replacement) {
				t.Fatalf("error %q must name %s and %s", err, legacy, replacement)
			}
		})
	}
}

func TestCheckLegacyEnvListsAllOffenders(t *testing.T) {
	t.Setenv(envnames.LegacyEnv, "dev")
	t.Setenv(envnames.LegacyAddr, ":9090")
	err := checkLegacyEnv()
	if err == nil {
		t.Fatal("expected error with two legacy vars set")
	}
	for _, want := range []string{envnames.LegacyEnv, envnames.Env, envnames.LegacyAddr, envnames.Addr} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must mention %s", err, want)
		}
	}
}

func TestCheckLegacyEnvIgnoresBlankValues(t *testing.T) {
	t.Setenv(envnames.LegacyEnv, "   ")
	if err := checkLegacyEnv(); err != nil {
		t.Fatalf("blank legacy value must be ignored, got %v", err)
	}
}
```

- [ ] **Step 2: Vérifier l'échec de compilation**

Run: `go test . -run TestCheckLegacyEnv 2>&1 | tail -3`
Expected: FAIL — `undefined: checkLegacyEnv`.

- [ ] **Step 3: Implémenter `env_guard.go`**

```go
package ovr

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

// checkLegacyEnv refuses to start while a retired PIP_* variable is still
// set, so a stale deployment fails loudly instead of silently losing its
// admin token or dev mode after the OUVRIER_* rename.
func checkLegacyEnv() error {
	names := make([]string, 0, len(envnames.Legacy))
	for legacy := range envnames.Legacy {
		names = append(names, legacy)
	}
	sort.Strings(names)
	offending := make([]string, 0, len(names))
	for _, legacy := range names {
		if strings.TrimSpace(os.Getenv(legacy)) == "" {
			continue
		}
		offending = append(offending, fmt.Sprintf("%s is no longer read; rename it to %s", legacy, envnames.Legacy[legacy]))
	}
	if len(offending) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to start: %s", strings.Join(offending, "; "))
}
```

- [ ] **Step 4: Vérifier le vert**

Run: `go test . -run TestCheckLegacyEnv -v 2>&1 | tail -10`
Expected: PASS sur les 4 tests.

- [ ] **Step 5: Commit**

```bash
git add env_guard.go env_guard_test.go
git commit -m "feat: fail-fast guard rejecting retired PIP_* env vars"
```

---

### Task 4: Câbler le garde-fou dans `Run()` et `Runner.Handler()`

**Files:**
- Modify: `runner.go:260-270` (`Run`)
- Modify: `handler.go:46-55` (`Runner.Handler`)
- Test: `env_guard_test.go` (ajout)

- [ ] **Step 1: Écrire les tests (rouges)**

Ajouter à `env_guard_test.go` :

```go
func TestRunRefusesLegacyEnv(t *testing.T) {
	t.Setenv(envnames.LegacyAdminToken, "old-secret")
	err := NewRunner().Run("127.0.0.1:0")
	if err == nil || !strings.Contains(err.Error(), envnames.AdminToken) {
		t.Fatalf("Run must refuse legacy env with migration hint, got %v", err)
	}
}

func TestHandlerRefusesLegacyEnv(t *testing.T) {
	t.Setenv(envnames.LegacyEnv, "dev")
	_, err := Handler()
	if err == nil || !strings.Contains(err.Error(), envnames.Env) {
		t.Fatalf("Handler must refuse legacy env with migration hint, got %v", err)
	}
}
```

(Le garde-fou s'exécute avant `compilePlans`, donc aucun node n'est requis : l'erreur attendue est celle de la migration, pas une erreur de compilation.)

- [ ] **Step 2: Vérifier le rouge**

Run: `go test . -run 'TestRunRefuses|TestHandlerRefuses' 2>&1 | tail -5`
Expected: FAIL — l'erreur retournée est l'erreur de compilation (« no nodes »), pas l'erreur de migration.

- [ ] **Step 3: Câbler dans `Run` (runner.go)**

Juste après le bloc `if r.err != nil { return r.err }` (ligne ~266), avant `compilePlans` :

```go
	if err := checkLegacyEnv(); err != nil {
		return err
	}
```

- [ ] **Step 4: Câbler dans `Runner.Handler` (handler.go)**

Juste après le bloc `if r.err != nil { return nil, r.err }` (ligne ~52), avant `compilePlans` :

```go
	if err := checkLegacyEnv(); err != nil {
		return nil, err
	}
```

- [ ] **Step 5: Vérifier le vert + non-régression racine**

Run: `go test . 2>&1 | tail -3`
Expected: `ok` — les nouveaux tests passent et aucun test existant ne pose de `PIP_*` (migrés en Task 2).

- [ ] **Step 6: Commit**

```bash
git add runner.go handler.go env_guard_test.go
git commit -m "feat: wire legacy env guard into Run and Runner.Handler"
```

---

### Task 5: Migration CLI (`status`, `logs`, `trace`, `dev`, `help`)

**Files:**
- Modify: `internal/cli/status.go:22,30,41-46`
- Modify: `internal/cli/logs.go:24,36`
- Modify: `internal/cli/trace.go:22,38`
- Modify: `internal/cli/dev.go:68,127,135,334-372`
- Modify: `internal/cli/help.go:80,90,101,195`
- Modify: `internal/cli/dev_test.go` (sed)
- Test: `internal/cli/status_test.go` (ajout)

- [ ] **Step 1: Écrire les tests (rouges)**

Ajouter à `internal/cli/status_test.go` (imports à compléter : `strings`, `github.com/ArnaudGuiovanna/ouvrier/internal/envnames`) :

```go
func TestResolveAdminTokenRejectsLegacyEnv(t *testing.T) {
	t.Setenv(envnames.AdminToken, "")
	t.Setenv(envnames.LegacyAdminToken, "old-secret")
	_, err := resolveAdminToken("")
	if err == nil || !strings.Contains(err.Error(), envnames.AdminToken) {
		t.Fatalf("expected migration error naming %s, got %v", envnames.AdminToken, err)
	}
}

func TestResolveAdminTokenPrecedence(t *testing.T) {
	t.Setenv(envnames.LegacyAdminToken, "old-secret")
	if got, err := resolveAdminToken("flag-token"); err != nil || got != "flag-token" {
		t.Fatalf("flag must win: got %q, %v", got, err)
	}
	t.Setenv(envnames.AdminToken, "new-secret")
	if got, err := resolveAdminToken(""); err != nil || got != "new-secret" {
		t.Fatalf("%s must win over legacy: got %q, %v", envnames.AdminToken, got, err)
	}
}
```

- [ ] **Step 2: Vérifier le rouge**

Run: `go test ./internal/cli -run TestResolveAdminToken 2>&1 | tail -3`
Expected: FAIL de compilation — `resolveAdminToken` retourne une seule valeur.

- [ ] **Step 3: Implémenter `resolveAdminToken` (status.go)**

Ajouter l'import `"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"` puis :

```go
func resolveAdminToken(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if v := os.Getenv(envnames.AdminToken); v != "" {
		return v, nil
	}
	if os.Getenv(envnames.LegacyAdminToken) != "" {
		return "", fmt.Errorf("%s is no longer read; rename it to %s", envnames.LegacyAdminToken, envnames.AdminToken)
	}
	return "", nil
}
```

- [ ] **Step 4: Adapter les trois appelants**

Dans `status.go:30`, `logs.go:36` et `trace.go:38`, remplacer `client := newAdminClient(*url..., resolveAdminToken(*token))` par :

```go
	adminToken, err := resolveAdminToken(*token)
	if err != nil {
		return err
	}
	client := newAdminClient(*urlFlag, adminToken)
```

(Dans `status.go` la variable s'appelle `*url`, dans `logs.go`/`trace.go` `*urlFlag` — garder le nom local existant.) Mettre à jour les descriptions de flag dans les trois fichiers : `"admin bearer token (defaults to $OUVRIER_ADMIN_TOKEN)"`.

- [ ] **Step 5: Migrer `dev.go`**

Ajouter l'import `envnames`. Quatre changements :
- ligne 68 : `flags.String("addr", "", "override the worker listen address via OUVRIER_ADDR")`
- lignes 127 et 135 : `fmt.Fprintf(out, "ouvrier dev: %s=%s\n", envnames.Addr, cfg.Addr)`
- commentaire de `devEnv` (ligne ~337) : `// override via OUVRIER_ADDR. .env values are never logged.`
- dans `devEnv`, remplacer la boucle de réécriture :

```go
		if kv[:eq] == envnames.Addr {
			out = append(out, envnames.Addr+"="+cfg.Addr)
			consumed = true
			continue
		}
```

et après la boucle :

```go
	if !consumed {
		out = append(out, envnames.Addr+"="+cfg.Addr)
	}
```

- [ ] **Step 6: Migrer `help.go` et les tests CLI**

Dans `help.go`, remplacer les 3 occurrences de `$PIP_ADMIN_TOKEN` (lignes 80, 90, 101) par `$OUVRIER_ADMIN_TOKEN` et `PIP_ADDR` (ligne 195) par `OUVRIER_ADDR`. Puis :

```bash
cd /home/ubuntu/ouvrier && sed -i 's/PIP_ADDR/OUVRIER_ADDR/g; s/PIP_ADMIN_TOKEN/OUVRIER_ADMIN_TOKEN/g; s/PIP_ENV/OUVRIER_ENV/g; s/PIP_LOG_LEVEL/OUVRIER_LOG_LEVEL/g' internal/cli/dev_test.go
```

- [ ] **Step 7: Vérifier le vert**

Run: `go test ./internal/cli 2>&1 | tail -3`
Expected: `ok`.

- [ ] **Step 8: Commit**

```bash
git add internal/cli
git commit -m "feat!: CLI reads OUVRIER_ADMIN_TOKEN/OUVRIER_ADDR, errors on legacy names"
```

---

### Task 6: Migration scaffold

**Files:**
- Modify: `internal/scaffold/templates.go:26,187-190`
- Modify: `internal/scaffold/scaffold_test.go` (sed)

- [ ] **Step 1: Migrer le template `main.go` généré**

Dans `mainGo` (templates.go:26), le code généré devient (littéral volontaire : le projet généré n'importe pas `internal/envnames`) :

```go
func listenAddr() string {
	if addr := os.Getenv("OUVRIER_ADDR"); addr != "" {
		return addr
	}
	return ":8080"
}
```

- [ ] **Step 2: Migrer le `.env` généré**

Dans `envExample` (templates.go:187-190) :

```go
	b.WriteString("OUVRIER_ENV=dev\n")
	b.WriteString("OUVRIER_ADDR=:8080\n")
	b.WriteString("OUVRIER_LOG_LEVEL=info\n")
	b.WriteString("OUVRIER_ADMIN_TOKEN=\n")
```

- [ ] **Step 3: Migrer les tests scaffold**

```bash
cd /home/ubuntu/ouvrier && sed -i 's/PIP_ENV/OUVRIER_ENV/g; s/PIP_ADMIN_TOKEN/OUVRIER_ADMIN_TOKEN/g; s/PIP_ADDR/OUVRIER_ADDR/g; s/PIP_LOG_LEVEL/OUVRIER_LOG_LEVEL/g' internal/scaffold/scaffold_test.go
```

- [ ] **Step 4: Vérifier le vert**

Run: `go test ./internal/scaffold 2>&1 | tail -3`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/scaffold
git commit -m "feat!: scaffolded projects use OUVRIER_* env vars"
```

---

### Task 7: Documentation

**Files:**
- Modify: `README.md`, `docs/handbook.md`, `docs/observability.md`, `examples/ticket-triage/README.md`, `examples/moodle-fsrs/README.md`

(Constat préalable : `docs/api.md`, `docs/ouvrier-syntax-handbook.md` et son PDF ne contiennent aucun `PIP_*` — rien à régénérer. `AGENTS.md` non plus.)

- [ ] **Step 1: Remplacement mécanique**

```bash
cd /home/ubuntu/ouvrier && sed -i 's/PIP_ENV/OUVRIER_ENV/g; s/PIP_ADMIN_TOKEN/OUVRIER_ADMIN_TOKEN/g; s/PIP_ADDR/OUVRIER_ADDR/g; s/PIP_LOG_LEVEL/OUVRIER_LOG_LEVEL/g' README.md docs/handbook.md docs/observability.md examples/ticket-triage/README.md examples/moodle-fsrs/README.md
```

- [ ] **Step 2: Relecture des diffs**

Run: `git diff --stat && git diff | grep -C1 'OUVRIER_' | head -60`
Expected: uniquement des renommages de variables d'env ; aucune phrase devenue incohérente (sinon corriger à la main).

- [ ] **Step 3: Vérifier l'absence de `PIP_*` résiduel en docs**

Run: `grep -rn 'PIP_' README.md AGENTS.md docs examples --include='*.md' | grep -v docs/superpowers`
Expected: aucune sortie.

- [ ] **Step 4: Commit**

```bash
git add README.md docs/handbook.md docs/observability.md examples
git commit -m "docs: rename PIP_* env vars to OUVRIER_*"
```

---

### Task 8: Validation finale (gates CI + critères du spec)

- [ ] **Step 1: Gates CI complets**

```bash
cd /home/ubuntu/ouvrier && gofmt -l . && go vet ./... && staticcheck ./... && go test ./... 2>&1 | tail -15
```

Expected: `gofmt -l` muet, vet/staticcheck muets, tous les packages `ok` (y compris `examples_build_test.go`).

- [ ] **Step 2: Critère du spec — plus aucun `PIP_*` hors envnames/tests du garde-fou**

```bash
grep -rn 'PIP_' --include='*.go' . | grep -v internal/envnames
```

Expected: aucune sortie (les tests du garde-fou utilisent les constantes `envnames.Legacy*`, pas de littéraux).

- [ ] **Step 3: Vérification fonctionnelle du garde-fou**

```bash
cd /home/ubuntu/ouvrier/examples/ticket-triage && PIP_ADMIN_TOKEN=x OUVRIER_ENV=dev go run . 2>&1 | head -2
```

Expected: le binaire refuse de démarrer avec `PIP_ADMIN_TOKEN is no longer read; rename it to OUVRIER_ADMIN_TOKEN`.

- [ ] **Step 4: Commit final (si retouches) et état git propre**

```bash
git status --short
```

Expected: arbre propre (chaque task a déjà committé).
