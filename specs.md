# Ouvrier — Spécifications du framework

**Version** : v0.1
**Statut** : spécification d'implémentation
**Auteur** : Arnaud Guiovanna

---

## 1. Vue d'ensemble

Ouvrier est un framework Go pour créer rapidement des **middlewares agentiques** : de petits services autonomes qui exploitent un LLM pour augmenter n'importe quelle API existante (LMS, CRM, SaaS, plateformes de support, etc.).

### 1.1 Promesse

Un développeur décrit un trigger, un goal, des tools. Ouvrier produit un binaire statique prêt à tourner en production. De l'idée au déploiement en quelques minutes.

### 1.2 Mental model

L'agent est une primitive de premier ordre, comme l'objet l'est en POO. Le framework s'inspire des pipes Unix : une chaîne séquentielle d'agents où chaque étape consomme la sortie de la précédente, sans glue code.

```
From → Pipe → Pipe → Pipe → Reply / Push / Sink
```

### 1.3 Tagline

**Workers for your APIs.**

### 1.4 Public cible

Développeur backend Go de niveau junior à intermédiaire. Pas un dev Go expert, pas un non-codeur. Quelqu'un qui sait écrire une fonction Go, qui connaît HTTP et JSON, mais qui n'a jamais déployé d'agent IA et veut le faire vite.

---

## 2. Architecture

### 2.1 Composants principaux

Le framework est composé de trois éléments distincts :

1. **Le runtime** : la bibliothèque Go importée par le projet utilisateur. Expose les primitives `From`, `Pipe`, `Run`, `Reply`, `Push`, `Sink`, etc.
2. **La CLI** : un binaire `ouvrier` installé sur la machine du développeur. Scaffolde, build, déploie, monitore.
3. **Le harnais agentique** : l'infrastructure interne qui exécute un Pipe (tool-use loop, gestion LLM, sandbox, retry, observabilité).

### 2.2 Nom de package

- Module Go : `github.com/yourorg/ouvrier`
- Package importé : `ovr` (déclaration `package ovr` à l'intérieur)
- Binaire CLI : `ouvrier`

### 2.3 Structure d'un projet utilisateur

Un projet généré par `ouvrier new` ressemble à :

```
mon-projet/
├── main.go              # déclaration du pipeline
├── go.mod
├── pip.yaml             # configuration de déploiement
├── .env                 # secrets (non commité)
├── .env.example         # template de secrets
├── .gitignore
├── README.md
├── skills/              # dossiers de skills .md
│   └── nom-skill/
│       └── SKILL.md
└── tools/               # fonctions Go custom
    └── nom-tool.go
```

---

## 3. Les 4 primitives

### 3.1 From — le trigger

Définit l'événement qui déclenche le pipeline. C'est toujours le premier argument de `Run`.

**Variantes supportées en v0** :

- `ovr.From("POST /chemin/{param}")` — endpoint HTTP, méthode + path
- `ovr.From("GET /chemin")` — endpoint HTTP GET
- `ovr.From(ovr.Cron("0 6 * * *"))` — planification cron
- `ovr.From(ovr.Webhook("provider"))` — webhook signé (Stripe, GitHub, etc.)
- `ovr.From(ovr.Stream("kafka://topic"))` — stream Kafka / NATS / Redis

**Options du From** :

- `ovr.WorkerPool(N)` — borne la concurrence sur stream/cron (défaut : 1)
- `ovr.IdempotencyKey("Header-Name")` — clé d'idempotence basée sur un header HTTP
- `ovr.VerifySignature("ENV_VAR", "Header-Name")` — vérifie une signature HMAC

### 3.2 Pipe — l'agent

Une étape du pipeline. Un agent autonome défini par un goal en langage naturel, un modèle LLM, et un ensemble de capacités.

**Signature** : `ovr.Pipe(goal string, options ...PipeOption)`

**Options obligatoires** : au moins un `Model(...)`.

**Options disponibles** :

- `ovr.Model("anthropic/claude-sonnet-4-6")` — modèle LLM, toujours explicite, jamais d'alias
- `ovr.Tool("name", goFunc)` — fonction Go enregistrée comme tool
- `ovr.Skill("dossier-name")` — référence un dossier `./skills/dossier-name/SKILL.md`
- `ovr.MCP("server-name")` — connecte un MCP server externe (URL dans .env)
- `ovr.Bash(ovr.Sandbox("/tmp/workdir"))` — shell sandboxé
- `ovr.Timeout("30s")` — borne temporelle du Pipe
- `ovr.Retry(3, ovr.ExponentialBackoff())` — politique de retry
- `ovr.NoCache()` — désactive le prompt caching pour ce Pipe
- `ovr.SequentialTools()` — force le LLM à appeler les tools en série

**Comportement** :

- Le Pipe reçoit l'outcome du Pipe précédent (ou le payload du trigger pour le premier)
- Le LLM raisonne via une tool-use loop interne
- Le Pipe produit son outcome qui alimente le Pipe suivant
- Sans `Output[T]()`, la sortie est un `map[string]any` non typé
- Avec `Output[T]()`, le runtime valide la sortie contre le schéma T

### 3.3 Run — le démarrage

Démarre le serveur HTTP, enregistre les crons, lance les workers de stream. Tous les pipelines sont déclarés dans un seul Run.

**Signature** : `ovr.Run(addr string, nodes ...Node) error`

**Comportement** :

- `addr` : adresse d'écoute HTTP (exemple `:8080`)
- `nodes` : liste de noeuds qui composent le pipeline (From + Pipes + Reply/Push/Sink)
- Démarre le serveur HTTP avec routes auto-générées
- Lance les crons et streams en background goroutines
- Bloque jusqu'à `SIGTERM` / `SIGINT`
- Retourne `nil` en shutdown propre, erreur sinon

### 3.4 Reply, Push, Sink — la sortie

Trois verbes selon ce qu'on veut faire de l'outcome final.

#### Reply — répondre à l'expéditeur

Utilisé quand le trigger est HTTP synchrone et qu'on veut renvoyer une réponse à l'appelant.

- `ovr.Reply(ovr.JSON[T]())` — réponse JSON typée
- `ovr.Reply(ovr.SSE())` — streaming Server-Sent Events
- `ovr.Reply(ovr.Accepted())` — HTTP 202, traitement asynchrone

#### Push — pousser vers un tiers

Utilisé pour envoyer le résultat vers un autre endpoint, asynchrone.

- `ovr.Push(ovr.Webhook("https://..."))` — POST HTTP sortant
- `ovr.Push(ovr.Queue("nats://..."))` — publish dans une queue

#### Sink — terminer sans retour

Utilisé quand le trigger est asynchrone (cron, stream) et que personne n'attend.

- `ovr.Sink(ovr.Log())` — fire-and-forget, log et métriques OTel
- `ovr.Sink(ovr.File("./out/result.json"))` — écriture sur disque

---

## 4. Composition et concurrence

### 4.1 Séquentiel par défaut

Sans primitive de composition, les Pipes s'exécutent dans l'ordre déclaré. Pipe 2 attend que Pipe 1 ait terminé.

### 4.2 Parallel — fan-out

Plusieurs Pipes reçoivent le même input et s'exécutent en parallèle. Le Pipe suivant reçoit un tableau des outcomes dans l'ordre déclaré.

```go
ovr.Parallel(
    ovr.Pipe("Évalue la qualité", ...),
    ovr.Pipe("Vérifie la conformité", ...),
    ovr.Pipe("Évalue le niveau", ...),
)
```

**Comportement** :

- Worker pool borné par défaut à `runtime.NumCPU()`
- Échec d'un Pipe → toute la `Parallel` échoue (fail-fast)
- Option `ovr.PartialOK()` pour tolérer les échecs partiels

### 4.3 Map — application sur collection

Le Pipe précédent a produit une liste. `Map` applique un sous-pipeline à chaque élément en parallèle, avec un parallélisme borné.

```go
ovr.Map(
    ovr.Concurrency(10),
    ovr.Pipe("Calcule X", ...),
    ovr.Pipe("Génère Y", ...),
)
```

### 4.4 WorkerPool — concurrence sur trigger

Option du `From` qui borne le nombre d'exécutions de pipeline simultanées.

```go
ovr.From("POST /webhooks", ovr.WorkerPool(20))
```

### 4.5 SubAgent — pipeline comme tool

Un pipeline peut être exposé comme tool à un autre Pipe. Le LLM parent décide quand l'invoquer.

```go
var translator = ovr.Pipeline(
    ovr.Pipe("Traduit le texte", ...),
)

ovr.Pipe("Rédige un email multilingue",
    ovr.SubAgent("translate", translator),
)
```

**Comportement par défaut** :

- Le LLM parent décide d'invoquer (mode `Auto`)
- Parallel tool calling activé
- Cap dur de 5 invocations parallèles maximum
- Override possible avec `ovr.MaxParallel(N)`

---

## 5. Tools, Skills, MCP

### 5.1 Tool — fonction Go déterministe

Une fonction Go enregistrée comme capacité d'un agent. Le framework infère son JSON schema via reflection au démarrage.

```go
func ListLearners(ctx context.Context, days int) ([]Learner, error) {
    // ...
}

ovr.Pipe("...", ovr.Tool("list_learners", ListLearners))
```

**Contraintes de signature** :

- Premier paramètre obligatoire : `context.Context`
- Paramètres suivants : types Go simples (string, int, float, bool, struct, slice, map)
- Retour : valeur + error, ou juste error
- Types complexes doivent avoir des tags `json:"..."` pour la sérialisation

**Options du Tool** :

- `ovr.Describe("description pour le LLM")` — surcharge la description inférée
- `ovr.Param("name", "description")` — décrit un paramètre spécifique

### 5.2 Skill — expertise en Markdown

Un dossier `./skills/nom-skill/` contenant un fichier `SKILL.md` au format Anthropic Agent Skills.

**Format du SKILL.md** :

```markdown
---
name: nom-skill
description: Description courte pour que le LLM sache quand l'utiliser.
---

# Instructions principales

Tu rédiges...

## Règles

1. Première règle
2. Deuxième règle

## Format de sortie

Retourne...
```

**Comportement** :

- Le frontmatter `name` et `description` est obligatoire
- Le corps Markdown est injecté dans le system prompt du LLM
- Les fichiers de support (`scripts/`, `references/`) peuvent être chargés à la demande via filesystem tool
- Les SKILL.md sont scannés au boot et embarqués dans le binaire via `go:embed`

### 5.3 MCP — Model Context Protocol

Connecte un MCP server externe comme set de tools.

```go
ovr.Pipe("...", ovr.MCP("moodle-mcp"))
```

**Configuration** :

- L'URL du MCP server est dans `.env` : `MOODLE_MCP_URL=https://...`
- Authentification optionnelle via `MOODLE_MCP_TOKEN`
- Le framework agit comme client MCP standard

### 5.4 Bash — shell sandboxé

```go
ovr.Pipe("Analyse les logs",
    ovr.Bash(ovr.Sandbox("/tmp/workspace")),
)
```

**Comportement** :

- Workspace isolé dans le dossier indiqué
- Pas d'accès en dehors du sandbox
- Timeout par défaut 30s par commande
- Environnement minimal (pas d'accès aux variables d'env du processus parent sauf liste blanche)

---

## 6. CLI

La CLI `ouvrier` est la porte d'entrée du framework. Elle est construite avec Charm Bracelet (`bubbletea`, `lipgloss`) pour une TUI élégante.

### 6.1 Commandes

#### `ouvrier new`

Scaffolding interactif d'un nouveau projet. Pose des questions, génère tous les fichiers.

**Étapes** :

1. Nom du projet
2. Description courte
3. Choix du trigger (HTTP / Cron / Webhook / Stream)
4. Path ou expression spécifique au trigger
5. Nombre d'agents dans le pipeline
6. Pour chaque agent : goal, modèle LLM, tools / skills / MCP
7. Pour chaque skill : édition du SKILL.md dans un éditeur multi-lignes
8. Format de sortie : Reply / Push / Sink + variante
9. Secrets `.env` : saisie masquée, chmod 600
10. Cible de déploiement : SSH / Docker / les deux

**Sortie** : projet complet généré, avec `main.go` fonctionnel.

#### `ouvrier add agent`

Ajoute un agent à un pipeline existant.

- Demande la position dans le pipeline
- Demande le goal
- Demande le modèle et les capacités
- Modifie `main.go` en insérant le nouveau `ovr.Pipe(...)` au bon endroit
- Revalide la cohérence du pipeline

#### `ouvrier add tool`

Génère le stub d'un nouveau tool Go.

- Demande nom, description, signature (entrées, sortie)
- Crée le fichier `tools/nom-tool.go` avec un stub à compléter
- Affiche un rappel pour l'enregistrer dans `main.go`

#### `ouvrier add skill`

Crée un nouveau skill.

- Demande nom (kebab-case), description
- Ouvre un éditeur multi-lignes pour rédiger le SKILL.md
- Crée le dossier `skills/nom-skill/` avec le fichier

#### `ouvrier show`

Affiche la topologie actuelle du pipeline en ASCII, avec les modèles, tools, skills par agent.

#### `ouvrier dev`

Lance le serveur en local avec :

- Hot-reload sur modification de `main.go`, `tools/`, `skills/`
- Trace viewer accessible sur `http://localhost:8080/dev`
- Endpoint admin `POST /admin/trigger` pour forcer une exécution
- Endpoint admin `GET /admin/health` pour santé
- Logs structurés sur stdout

#### `ouvrier build`

Compile un binaire statique pour la cible.

- Par défaut : OS / arch courant
- Option `--target linux/amd64` etc.
- Option `--static` : binaire totalement statique (CGO_ENABLED=0)
- Option `--embed-skills` : embarque les SKILL.md via `go:embed` (par défaut activé)
- Sortie : `./bin/<nom-projet>`

#### `ouvrier deploy ssh`

Déploie via SSH sur un serveur Linux.

**Étapes internes** :

1. Build linux/amd64 statique
2. Connexion SSH au host (config dans `pip.yaml`)
3. scp du binaire vers le path cible
4. scp du `.env` (chmod 0600)
5. Génération / mise à jour de l'unité systemd
6. `systemctl restart` du service
7. Poll de `GET /admin/health` jusqu'à 200 ou timeout 30s
8. Rollback automatique si health check échoue

#### `ouvrier deploy docker`

Génère une image OCI distroless.

- Crée un Dockerfile minimal (base distroless)
- Build de l'image
- Tag avec la version dans `pip.yaml`
- Push optionnel vers un registry

#### `ouvrier status [host]`

Inspecte un déploiement à distance.

- Sans argument : inspecte localhost
- Avec host : se connecte au `/admin/status` du déployé
- Affiche : uptime, dernières N exécutions, taux de succès, conformité schéma, coût LLM cumulé

#### `ouvrier logs [host]`

Tail des logs d'un déployé.

- Options : `--tail`, `--since 1h`, `--follow`

#### `ouvrier trace [host]`

Affiche les dernières exécutions avec leurs spans.

- `--last N` : N dernières exécutions
- `<exec-id>` : détail d'une exécution spécifique avec ses spans, inputs, outputs

### 6.2 Style visuel TUI

- Couleurs : noir, blanc, vert phosphore (#00d27a) comme accent unique
- Police : monospace (par défaut celle du terminal de l'utilisateur)
- Encadrés ASCII Unicode (`┌─┐ │ └─┘`)
- Indicateurs visuels : `✓` vert, `✗` rouge, `⚠` jaune
- Pas d'animations gratuites, pas de spinners excessifs
- Style cohérent avec le manifeste de la doc

---

## 7. Configuration

### 7.1 Fichier `.env`

Contient tous les secrets et la configuration sensible. Jamais commité.

**Variables systématiquement attendues** :

- `ANTHROPIC_API_KEY` (ou autre provider LLM)
- `PIP_ENV` : `dev` / `staging` / `production`
- `PIP_PORT` : port d'écoute (override l'argument de Run)
- `PIP_LOG_LEVEL` : `debug` / `info` / `warn` / `error`
- `PIP_ADMIN_TOKEN` : token pour les endpoints admin (`/admin/*`)

**Variables spécifiques au projet** :

- URLs des MCP servers (`MOODLE_MCP_URL`, etc.)
- Tokens d'APIs externes (`MOODLE_TOKEN`, `STRIPE_API_KEY`, etc.)
- URLs de queues (`REDIS_URL`, `NATS_URL`, etc.)

**Validation au boot** : utiliser `ovr.RequireEnv("VAR1", "VAR2", ...)` dans `main.go` pour valider la présence des variables au démarrage. Fail-fast si manquante.

### 7.2 Fichier `pip.yaml`

Contient la configuration de build et déploiement. Commité dans Git.

```yaml
name: mon-projet
version: 0.1.0

deploy:
  ssh:
    host: ops@server.example.com
    path: /opt/mon-projet
    service: mon-projet.service
    healthcheck:
      path: /admin/health
      timeout: 30s
  docker:
    image: registry.example.com/mon-projet
    base: gcr.io/distroless/static-debian12

env:
  required:
    - ANTHROPIC_API_KEY
    - MOODLE_BASE_URL

healthcheck:
  path: /admin/health
  interval: 30s
```

### 7.3 Variables d'environnement vs `pip.yaml`

| Type | Fichier | Commité |
|---|---|---|
| Secrets, tokens, URLs sensibles | `.env` | Non |
| Config build / deploy | `pip.yaml` | Oui |
| Template de référence | `.env.example` | Oui |

---

## 8. Harnais agentique

Le harnais est l'infrastructure invisible qui exécute un Pipe. L'utilisateur ne l'écrit jamais. Il fournit :

### 8.1 Tool-use loop

- Itération entre le LLM et les tools jusqu'à atteindre le goal
- Max iterations configurable (défaut 25)
- Max tokens configurable (défaut 500_000)
- Max coût USD configurable (défaut 5.00)
- Si limite atteinte → outcome partiel avec statut `truncated`

### 8.2 Parallel tool calling

- Activé par défaut pour les modèles qui le supportent (Claude 3+, GPT-4+)
- Désactivable avec `ovr.SequentialTools()`
- Tools en parallèle exécutés en goroutines, attente de tous

### 8.3 Prompt caching

- Hash de la partie statique du prompt (system + tools + skill) calculé au boot
- Cache key envoyé au provider LLM
- Désactivable avec `ovr.NoCache()`

### 8.4 Retry et erreurs

- Erreurs transitoires (5xx, network, rate limit) → retry avec backoff exponentiel
- Erreurs permanentes (4xx, validation) → fail immédiat
- 3 retries par défaut, override avec `ovr.Retry(N, backoff)`

### 8.5 Observabilité

- OpenTelemetry instrumenté automatiquement
- Un span par Pipe, un span par tool call, un span par LLM call
- Attributs : tokens input / output, cost USD, latency ms
- Exportable vers Datadog, Grafana, Honeycomb, etc.

### 8.6 Budgets globaux

Par exécution de pipeline :

- Max tokens : 500_000 par défaut
- Max coût : 5.00 USD par défaut
- Max wallclock : 10 minutes par défaut
- Override possible via options de `Run` ou par Pipe

---

## 9. Providers LLM

### 9.1 Provider supporté en v0.1

**Anthropic Claude uniquement**, via endpoint `/v1/messages`.

Modèles supportés en v0.1 :

- `anthropic/claude-opus-4-7`
- `anthropic/claude-sonnet-4-6`
- `anthropic/claude-haiku-4-5`

### 9.2 Architecture provider

Une interface `provider.Provider` permet d'ajouter d'autres providers ultérieurement (OpenAI, Google, Ollama). Pas implémenté en v0.1 mais structure préparée.

### 9.3 Authentification

Lecture de `ANTHROPIC_API_KEY` depuis `.env`. Pas de support de profils multiples en v0.1.

---

## 10. Monitoring et santé

### 10.1 Métriques exposées

Deux métriques essentielles en v0 :

**Santé fonctionnelle** : chaque exécution traverse-t-elle tous les Pipes sans erreur ?

- Compteur : exécutions tentées
- Compteur : exécutions réussies
- Compteur : exécutions en erreur (avec breakdown par Pipe)

**Santé qualité** : chaque outcome respecte-t-il son schéma déclaré ?

- Compteur : outcomes validés
- Compteur : violations de schéma
- Liste des dernières violations avec contexte

### 10.2 Endpoints admin

Exposés automatiquement sur le serveur du déployé, protégés par `PIP_ADMIN_TOKEN` :

- `GET /admin/health` — liveness + dernières N exécutions
- `GET /admin/status` — santé fonctionnelle + qualité, coûts
- `GET /admin/traces?last=N` — dernières exécutions avec spans
- `GET /admin/traces/<exec-id>` — détail d'une exécution
- `POST /admin/trigger` — force une exécution (utile pour cron en debug)

### 10.3 Trace viewer (dev uniquement)

En mode `ouvrier dev`, `GET /dev` expose une UI web autonome :

- Topologie visuelle du pipeline
- Liste des exécutions live avec Gantt chart
- Détail d'une exécution : input, output, tool calls par Pipe
- Coût LLM par exécution et cumulé

---

## 11. Sécurité

### 11.1 Secrets

- `.env` jamais commité (ajouté automatiquement à `.gitignore` par `ouvrier new`)
- Chmod 0600 systématique sur les `.env` uploadés
- Pas de secrets dans les binaires (vérifié au build)
- Pas de secrets dans les logs (champs sensibles masqués)

### 11.2 Endpoints admin

- Tous protégés par `PIP_ADMIN_TOKEN` via header `Authorization: Bearer <token>`
- Token non logué, non exposé dans les traces
- 401 si token absent, 403 si token incorrect

### 11.3 Webhooks signés

`ovr.VerifySignature("ENV_VAR", "Header-Name")` :

- Vérifie la signature HMAC du payload contre le secret dans `.env`
- Algorithme par défaut : HMAC-SHA256
- 401 si signature absente, 403 si invalide

### 11.4 Sandbox bash

- Workspace isolé par projet
- Pas d'accès filesystem en dehors du workspace
- Variables d'environnement filtrées (whitelist)
- Pas d'accès réseau par défaut (override possible)

---

## 12. Déploiement

### 12.1 Cibles supportées en v0.1

- **SSH** : binaire + systemd sur un Linux quelconque
- **Docker** : image OCI distroless

### 12.2 Caractéristiques du binaire

- Statique (CGO_ENABLED=0)
- Taille typique : 10-20 MB selon les skills embarquées
- Skills embarqués via `go:embed`
- Pas de dépendances runtime (pas de libc, pas de Node, pas de Python)

### 12.3 SSH deploy

- Connexion via `golang.org/x/crypto/ssh`
- Auth par clé SSH par défaut (clé du user)
- Upload du binaire + `.env` en chmod 0600
- Génération automatique d'unité systemd (`<name>.service`)
- `systemctl restart` + healthcheck

### 12.4 Docker deploy

- Génération automatique du Dockerfile distroless
- Build local avec `docker build`
- Push optionnel vers un registry
- L'utilisateur déploie l'image lui-même sur son orchestrateur

### 12.5 Gestion des secrets en prod

**En SSH** : `.env` uploadé en clair sur le serveur cible (chmod 0600). Documenté comme limitation. Pour les déploiements à enjeu, recommandation d'utiliser Vault, SOPS, Doppler en complément (non fourni en v0.1).

**En Docker** : pas de `.env` dans l'image. Variables passées via `--env-file` ou via les secrets de l'orchestrateur (Kubernetes Secrets, Fly secrets, etc.).

---

## 13. Limites assumées en v0.1

Documentées explicitement dans la doc utilisateur :

- LLM non-déterministes : validation des outcomes (schéma), pas du raisonnement
- Pas conçu pour l'expérimentation interactive (pas de notebooks)
- Écosystème Go vs Python/TS : moins d'exemples communautaires
- Secrets en SSH simple : pas de secret manager intégré
- Pas de visual builder, pas de drag-drop
- Optimisations conjoncturelles (prompt caching, parallel tool calling) finiront commodifiées par les SDKs officiels

---

## 14. Roadmap au-delà de v0.1

Listée pour clarifier ce qui est **hors scope** de la v0.1.

- DAG complets (fan-out / fan-in / branching conditionnel non-linéaire)
- Providers LLM additionnels (OpenAI, Google Gemini, Ollama)
- Transport distribué multi-process (NATS, gRPC, K8s operator)
- Marketplace de pipelines réutilisables
- UI web pour non-codeurs (générant du Go derrière)
- Pipeline as MCP server (`ExposeAsMCP`)
- Persistence des belief / sessions (au-delà de l'idempotency en mémoire)
- Mode stateless serverless (Cloudflare Workers, Lambda)
- DSL textuel séparé (`.agent` files)
- Loader YAML alternatif au Go
- WASM target via TinyGo

---

## 15. Critères de succès v0.1

Le framework est considéré comme livrable v0.1 si :

1. `ouvrier new` génère un projet qui compile et tourne en `ouvrier dev` sans erreur
2. Un pipeline à 3 Pipes (avec Tool, Skill, MCP) s'exécute de bout en bout
3. Le binaire `ouvrier deploy ssh` arrive en production avec health check OK
4. Le trace viewer affiche les exécutions et les coûts
5. La documentation utilisateur (le PDF) suffit à un dev junior pour démarrer seul
6. Les deux exemples de référence (Moodle FSRS + tickets triage) tournent

---

## 16. Conventions de code

### 16.1 Style Go

- `gofmt` strict
- Linter `staticcheck` activé
- Pas de variables globales sauf constantes
- Erreurs wrappées avec `fmt.Errorf("contexte: %w", err)`
- Contextes propagés partout (`context.Context` premier paramètre)

### 16.2 Convention de package

- Tous les imports en préfixe court `ovr.` (sauf `import .` documenté comme option avancée)
- Pas d'exports inutiles : ce qui n'est pas dans l'API publique reste minuscule

### 16.3 Tests

- Tests unitaires pour le runtime (`*_test.go`)
- Tests d'intégration pour la CLI (génération de projets test, build, dev)
- Pas de tests E2E avec déploiement réel en v0.1

---

## 17. Identité visuelle et documentation

- **Nom du framework** : Ouvrier
- **Tagline** : Workers for your APIs.
- **Palette** : noir (#0a0a0a), blanc cassé (#fafafa), vert phosphore (#00d27a)
- **Police** : monospace dominante (DejaVu Sans Mono, JetBrains Mono, ou équivalent)
- **Style** : TUI, sobre, design du vide, encadrés ASCII, terminologie système
- **Public** : développeurs Go juniors et intermédiaires
- **Format documentation** : Markdown + PDF généré

---

## 18. Inspirations et références

- **Unix pipes** : modèle de composition séquentielle
- **Anthropic Agent Skills** : format des SKILL.md (octobre 2025)
- **Model Context Protocol** : standard ouvert pour les tools
- **Flue framework** (TypeScript) : modèle de harnais et déploiement
- **Charm Bracelet** : librairie TUI Go
- **Aguiovanna.fr** : DA et tonalité

---

**Fin des spécifications v0.1**
