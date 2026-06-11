# Design : migration des variables d'environnement `PIP_*` → `OUVRIER_*`

**Date** : 2026-06-11
**Statut** : approuvé
**Type** : breaking change (pré-1.0, pas de rétrocompatibilité)

## Contexte

Le runtime, la CLI et le scaffold lisent quatre variables d'environnement héritées
du nom de projet initial : `PIP_ENV`, `PIP_ADMIN_TOKEN`, `PIP_ADDR`,
`PIP_LOG_LEVEL`. La convention `OUVRIER_*` existe déjà (`OUVRIER_ADMIN_INSECURE`,
`OUVRIER_STATE_BACKEND`, `OUVRIER_STATE_PATH`). L'audit DX avait flaggé cette
incohérence. Le préfixe `PIP_*` entre de plus en collision avec l'espace de noms
des variables d'environnement de pip (Python).

Décisions de cadrage (validées) :
- **Périmètre** : migration des noms seulement. Aucun changement d'auth admin
  (scopes, JWT) — hors périmètre.
- **Rétrocompatibilité** : aucune. Rename sec ; les anciens noms ne sont plus lus.
- **Approche** : constantes centralisées + garde-fou fail-fast au démarrage.

## Mapping

| Legacy (supprimé) | Nouveau (canonique) | Sémantique |
|---|---|---|
| `PIP_ENV` | `OUVRIER_ENV` | `dev` active le mode dev (viewer `/dev`, etc.) — inchangée |
| `PIP_ADMIN_TOKEN` | `OUVRIER_ADMIN_TOKEN` | bearer token des endpoints `/admin/*` — inchangée |
| `PIP_ADDR` | `OUVRIER_ADDR` | adresse d'écoute du worker — inchangée |
| `PIP_LOG_LEVEL` | `OUVRIER_LOG_LEVEL` | niveau de log — inchangée |

`OUVRIER_ADMIN_INSECURE` et `OUVRIER_STATE_*` sont déjà conformes : intouchés.

## Architecture

### 1. Package `internal/envnames`

Nouveau package minimal, seule source de vérité pour les noms de variables
d'environnement runtime :

```go
package envnames

const (
    Env        = "OUVRIER_ENV"
    AdminToken = "OUVRIER_ADMIN_TOKEN"
    Addr       = "OUVRIER_ADDR"
    LogLevel   = "OUVRIER_LOG_LEVEL"
)

// Legacy mappe chaque ancien nom (plus jamais lu) vers son remplaçant,
// pour le garde-fou de démarrage et les messages d'erreur.
var Legacy = map[string]string{
    "PIP_ENV":         Env,
    "PIP_ADMIN_TOKEN": AdminToken,
    "PIP_ADDR":        Addr,
    "PIP_LOG_LEVEL":   LogLevel,
}
```

Consommateurs : `provider_env.go` (token), `routes.go` (mode dev, messages
d'erreur), `internal/cli/{dev,status,logs,trace}.go`,
`internal/scaffold/templates.go`. Après migration, plus aucun nom de variable
d'env runtime en littéral hors de ce package (les textes d'aide de
`internal/cli/help.go` citent les noms dans des chaînes de doc, mis à jour
manuellement).

### 2. Garde-fou fail-fast `checkLegacyEnv()`

Fonction appelée au démarrage du runtime, au même point que
`checkAdminExposure` (couvre HTTP, cron, stream, mixed et le seam `Handler()`).
Pour chaque entrée de `envnames.Legacy` définie et non vide dans
l'environnement, le démarrage échoue :

```
refusing to start: PIP_ADMIN_TOKEN is no longer read; rename it to OUVRIER_ADMIN_TOKEN
```

Toutes les variables fautives sont listées dans une seule erreur (pas
d'arrêt à la première). Justification : sans garde-fou, un déploiement
existant redémarrerait avec `PIP_ADMIN_TOKEN` ignoré — `checkAdminExposure`
bloquerait le pire cas (bind réseau sans token), mais `PIP_ENV=dev` ignoré
changerait le comportement silencieusement. Fail-fast cohérent avec la
philosophie du projet (sandbox, validation à la compilation).

### 3. CLI

- `status`, `logs`, `trace` : le défaut de `--token` devient
  `$OUVRIER_ADMIN_TOKEN`. Si la nouvelle variable est absente mais
  `PIP_ADMIN_TOKEN` présent, la commande échoue avec le même message de
  migration que le garde-fou runtime.
- `dev` : injecte `OUVRIER_ADDR` (au lieu de `PIP_ADDR`) dans l'environnement
  du worker relancé ; la réécriture du `.env` chargé en mode dev filtre/remplace
  la clé `OUVRIER_ADDR`.
- `help.go` : textes d'aide mis à jour.

### 4. Scaffold

`internal/scaffold/templates.go` : le `main.go` généré lit `OUVRIER_ADDR` ;
le `.env` généré contient `OUVRIER_ADDR=:8080` et `OUVRIER_LOG_LEVEL=info`.

## Tests

- Migration mécanique des usages `PIP_*` existants dans les fichiers de test
  (~36 occurrences, dont `admin_exposure_test.go`, `runtime_http_*_test.go`,
  `scaffold_test.go`, `dev_test.go`).
- Nouveaux tests du garde-fou :
  - chaque variable legacy définie individuellement → le démarrage échoue et
    le message contient le nouveau nom ;
  - plusieurs variables legacy → toutes listées dans l'erreur ;
  - seuls les `OUVRIER_*` définis → démarrage normal ;
  - variable legacy définie mais vide → ignorée (pas d'erreur).
- Test CLI : `status` sans `--token`, avec `PIP_ADMIN_TOKEN` défini et
  `OUVRIER_ADMIN_TOKEN` absent → erreur de migration.

## Documentation

Mise à jour de toutes les occurrences `PIP_*` : `README.md`, `docs/api.md`,
`docs/handbook.md`, `docs/ouvrier-syntax-handbook.md`, `AGENTS.md`, exemples.
Le PDF du handbook est régénéré si un script de génération existe dans le
repo ; sinon, suivi manuel signalé à la fin de l'implémentation. Le commit
porte la mention `BREAKING CHANGE` avec le mapping complet.

## Hors périmètre

- Renforcement de l'auth admin (tokens scopés, JWT) — décision explicite.
- Toute rétrocompatibilité ou période de dépréciation.
- `OUVRIER_ADMIN_INSECURE`, `OUVRIER_STATE_BACKEND`, `OUVRIER_STATE_PATH`
  (déjà conformes).

## Critères de succès

1. `grep -r "PIP_" --include="*.go"` ne retourne plus que les entrées de
   `envnames.Legacy` et leurs tests.
2. `go test ./...`, `go vet`, `staticcheck` verts ; les exemples compilent.
3. Un binaire démarré avec un ancien `PIP_*` défini refuse de démarrer avec
   un message indiquant le nouveau nom.
4. La doc et le scaffold ne mentionnent plus aucun nom `PIP_*` (hors note de
   migration éventuelle).
