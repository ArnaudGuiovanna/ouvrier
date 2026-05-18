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

Invariant produit : **trigger, goal, tools, outcome**. Un développeur doit pouvoir rester à ce niveau mental pour 90% des usages. Le harnais agentique SOTA est une commodité fournie par Ouvrier : il est toujours actif par défaut, mais il ne doit pas rendre la syntaxe nominale plus complexe.

La syntaxe par défaut doit rester proche de :

```go
ovr.Run(":8080",
    ovr.From("POST /tickets"),
    ovr.Pipe("Triage le ticket",
        ovr.Model("anthropic/claude-sonnet-4-6"),
        ovr.Tool("load_ticket", LoadTicket),
        ovr.Output[Triage](),
    ),
    ovr.Reply(ovr.JSON[Triage]()),
)
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
3. **Le harnais agentique SOTA** : l'infrastructure interne qui exécute un Pipe. Il doit être comparable dans ses garanties de runtime aux harnais modernes de type Flue, Claude Code SDK ou Codex : session, tools réels, sandbox, permissions, hooks, events, state, schema, subagents.

### 2.1.1 Packages internes du harnais

Le modèle public reste simple (`From -> Pipe -> Reply/Push/Sink`), mais le runtime interne est découpé en packages maintenables :

- `internal/runtime` — compile les déclarations `Node` en plan exécutable, crée les exécutions, enchaîne Pipes, `Parallel`, `Map`, sorties.
- `internal/harness` — orchestre l'exécution d'un Pipe : session, budgets, boucle LLM/tools, retry, résultat final.
- `internal/tools` — registry et `ToolExecutor` unique pour Go tools, MCP tools, Bash, fichiers sandboxés, SubAgent.
- `internal/sandbox` — workspace, filesystem, env, process, réseau ; fail-fast si une garantie demandée ne peut pas être appliquée.
- `internal/policy` — `PermissionPolicy` déterministe pour filesystem, env, réseau, process, side effects, MCP et SubAgent.
- `internal/events` — `HookBus` et `EventStream`, source unique des traces, logs, SSE, admin et dev viewer.
- `internal/state` — `StateStore` pour historique d'exécution, sessions runtime, idempotence, traces, violations de schéma.
- `internal/schema` — `ResultSchema`, génération JSON Schema depuis Go, validation stricte et repair borné.
- `internal/provider` — frontière LLM : Anthropic Messages, tool use, prompt caching, coûts, classification d'erreurs.

Ces packages ne sont pas l'API utilisateur normale. L'utilisateur décrit un trigger, des goals, des tools et un outcome ; Ouvrier fournit le harnais automatiquement.

### 2.1.2 Surfaces avancées opt-in

Certains points du harnais sont configurables publiquement pour les usages avancés, sans devenir obligatoires :

- `ovr.NewRunner(...)` — configuration runtime avancée
- `ovr.WithStateStore(...)` — backend state store personnalisé
- `ovr.WithPermissionPolicy(...)` — policy production spécifique
- `ovr.WithHooks(...)` — hooks internes observables/testables
- `ovr.Sandbox(...)`, `ovr.AllowEnv(...)`, `ovr.AllowNetwork(...)` — sandbox explicite
- `ovr.ReadOnly()`, `ovr.SideEffecting(...)`, `ovr.Idempotent(...)` — classification des tools
- `ovr.Pipeline(...)`, `ovr.SubAgent(...)`, `ovr.MaxParallel(...)` — tâches enfants gouvernées

Les implémentations concrètes de `Harness`, `Session`, `ToolExecutor`, `EventStream`, `StateStore` et `ResultSchema` restent internes. Elles ne doivent être exposées directement que si un besoin produit réel apparaît.

### 2.2 Nom de package

- Module Go : `ouvrier` en développement local, chemin public final à figer avant release. Le placeholder `github.com/yourorg/ouvrier` est interdit en v0.1 livrable.
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
- `ovr.Output[T]()` — schéma de résultat typé du Pipe, validé par `ResultSchema`
- `ovr.Tool("name", goFunc)` — fonction Go enregistrée comme tool
- `ovr.Skill("dossier-name")` — référence un dossier `./skills/dossier-name/SKILL.md`
- `ovr.MCP("server-name")` — connecte un MCP server externe (URL dans .env)
- `ovr.Bash(ovr.Sandbox("/tmp/workdir"))` — shell sandboxé
- `ovr.Timeout("30s")` — borne temporelle du Pipe
- `ovr.Retry(3, ovr.ExponentialBackoff())` — politique de retry
- `ovr.NoCache()` — désactive le prompt caching pour ce Pipe
- `ovr.SequentialTools()` — force le LLM à appeler les tools en série
- `ovr.MaxCostUSD(5.00)` — budget coût du Pipe
- `ovr.MaxTokens(500_000)` — budget tokens du Pipe
- `ovr.PermissionPolicy(policy)` — politique avancée optionnelle, sinon défaut sécurisé

**Comportement** :

- Le Pipe reçoit l'outcome du Pipe précédent (ou le payload du trigger pour le premier)
- Le Pipe démarre toujours une `Session` interne rattachée à l'exécution de pipeline
- Le LLM raisonne via le harnais SOTA interne (`Harness`) et sa tool-use loop
- Tous les tool calls passent par `ToolExecutor`, `PermissionPolicy`, `HookBus`, `EventStream` et `StateStore`
- Le Pipe produit son outcome qui alimente le Pipe suivant
- Sans `Output[T]()`, la sortie est un `map[string]any` non typé
- Avec `Output[T]()`, le runtime valide la sortie contre le schéma T
- En cas de violation de schéma, le harnais peut tenter un repair LLM borné par budget ; toute violation est enregistrée dans `StateStore` et `EventStream`
- Une erreur, un timeout, une permission refusée ou un budget dépassé produit un outcome structuré et observable

### 3.3 Run — le démarrage

Démarre le serveur HTTP, enregistre les crons, lance les workers de stream. Tous les pipelines sont déclarés dans un seul Run.

**Signature** : `ovr.Run(addr string, nodes ...Node) error`

**API avancée** :

```go
runner := ovr.NewRunner(
    ovr.WithStateStore(store),
    ovr.WithPermissionPolicy(policy),
    ovr.WithHooks(hooks),
)
err := runner.Run(":8080", nodes...)
```

**Comportement** :

- `addr` : adresse d'écoute HTTP (exemple `:8080`)
- `nodes` : liste de noeuds qui composent le pipeline (From + Pipes + Reply/Push/Sink)
- Compile les `nodes` en plan exécutable via `internal/runtime`
- Crée une exécution avec `ExecID`, `SessionID`, `EventStream`, `StateStore`, budgets et policy par défaut
- Démarre le serveur HTTP avec routes auto-générées
- Lance les crons et streams en background goroutines
- Bloque jusqu'à `SIGTERM` / `SIGINT`
- Retourne `nil` en shutdown propre, erreur sinon
- `ovr.Run` utilise un runner par défaut sécurisé : state mémoire, permission policy restrictive, hooks vides, event stream local
- Le runner avancé ne doit jamais permettre de contourner `ToolExecutor`, `PermissionPolicy`, secret redaction ou `ResultSchema`

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

Un pipeline peut être exposé comme tool à un autre Pipe. Le LLM parent décide quand l'invoquer, mais l'exécution se fait comme une `Task` gouvernée par le harnais.

```go
var translator = ovr.Pipeline(
    ovr.Pipe("Traduit le texte",
        ovr.Model("anthropic/claude-haiku-4-5"),
        ovr.Output[Translation](),
    ),
)

ovr.Pipe("Rédige un email multilingue",
    ovr.Model("anthropic/claude-sonnet-4-6"),
    ovr.SubAgent("translate", translator),
)
```

**Comportement par défaut** :

- `SubAgent` est un tool adapter exécuté par `ToolExecutor`
- Chaque invocation crée une session enfant avec `ParentSessionID`
- Les budgets tokens/coût/wallclock sont hérités et bornés depuis la session parent
- Cancellation parent → cancellation enfants
- Profondeur maximale et détection de cycle obligatoires
- Cap dur de 5 invocations parallèles maximum
- Override possible avec `ovr.MaxParallel(N)`
- Les outcomes sont retournés dans l'ordre des appels ; `PartialOK()` peut tolérer certains échecs
- Tous les événements enfants sont rattachés à la trace parent

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
- `ovr.ReadOnly()` — le tool n'a pas de side effect et peut être retry/parallélisé
- `ovr.SideEffecting(...)` — le tool modifie un système externe (DB, email, webhook, fichier, etc.)
- `ovr.Idempotent("key-expression")` — side effect rejouable sans duplication
- `ovr.RequiresApproval()` — interdit en production sans policy explicite
- `ovr.ToolTimeout("10s")` — timeout du tool

**Harnais** :

- Le framework génère un JSON Schema d'input via reflection
- Les arguments LLM sont validés avant l'appel Go
- Tout appel passe par `ToolExecutor`, `PermissionPolicy`, `HookBus`, `EventStream` et `StateStore`
- Un panic de tool est converti en erreur structurée
- Les retries de tools ne sont autorisés que pour `ReadOnly()` ou `Idempotent(...)`
- Les tools sans classification explicite sont traités comme side-effecting non idempotents : pas de parallel tool calling automatique, pas de retry tool

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
- Les fichiers de support (`scripts/`, `references/`) peuvent être chargés à la demande via filesystem tool sandboxé
- Tout accès aux fichiers de support passe par `Sandbox` et `PermissionPolicy`
- Les SKILL.md sont scannés au boot et embarqués dans le binaire via `go:embed`
- Une Skill ne peut pas accéder directement aux secrets ; seules les capabilities déclarées et autorisées sont visibles

### 5.3 MCP — Model Context Protocol

Connecte un MCP server externe comme set de tools.

```go
ovr.Pipe("...", ovr.MCP("moodle-mcp"))
```

**Configuration** :

- L'URL du MCP server est dans `.env` : `MOODLE_MCP_URL=https://...`
- Authentification optionnelle via `MOODLE_MCP_TOKEN`
- Le framework agit comme client MCP standard
- L'implémentation Go utilise le SDK officiel
  `github.com/modelcontextprotocol/go-sdk/mcp` comme base protocolaire. Ouvrier
  peut l'encapsuler pour imposer `ToolExecutor`, `PermissionPolicy`, `Sandbox`,
  `EventStream` et `StateStore`, mais ne maintient pas un client MCP maison pour
  les transports et méthodes de base.

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
- Réseau refusé par défaut
- stdout/stderr bornés et tronqués avec événement observable
- Process group tué au timeout
- Fail-fast au boot si la plateforme ne permet pas de garantir l'isolation demandée
- En Docker distroless, `Bash` exige une image/runtime compatible ou échoue explicitement ; le binaire Ouvrier reste statique, mais la capability Bash dépend du runtime cible

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

### 8.1 Contrat SOTA non négociable

Le harnais Ouvrier v0.1 doit inclure dix composants internes obligatoires :

1. **Harness** — coordinateur d'un Pipe : prompt, provider, tools, schema, budgets, retry, events.
2. **Session** — état par exécution : `ExecID`, `SessionID`, `ParentSessionID`, messages, inputs/outputs, budgets, trace IDs, cancellation.
3. **ToolExecutor** — seule porte d'exécution des Go tools, MCP tools, Bash, fichiers sandboxés et SubAgent tasks.
4. **Sandbox** — isolation filesystem/env/process/réseau pour capabilities risquées.
5. **PermissionPolicy** — autorisation déterministe pour chaque action privilégiée.
6. **HookBus** — hooks internes autour des prompts, LLM calls, tool calls, schémas, budgets, subagents.
7. **EventStream** — flux append-only d'événements structurés.
8. **StateStore** — historique d'exécution, sessions runtime, traces, idempotence, violations de schéma.
9. **ResultSchema** — génération JSON Schema, validation stricte, repair borné.
10. **SubAgent/Task** — sous-pipelines gouvernés par sessions enfants et budgets hérités.

Ces composants sont obligatoires pour l'implémentation, mais ils ne sont pas tous des primitives publiques. Le chemin nominal reste `From`, `Pipe`, `Tool`, `Skill`, `Output`, `Reply/Push/Sink`. Le harnais est automatique par défaut ; les surfaces avancées servent uniquement aux utilisateurs qui veulent remplacer un store, durcir une policy, ajouter des hooks, configurer une sandbox ou gouverner des subagents.

### 8.2 Session

- Une `Session` est créée pour chaque exécution de Pipe
- Une exécution de pipeline a un `ExecID` stable
- Une session enfant est créée pour chaque `SubAgent` / `Task`
- Les messages LLM, tool calls, tool results, budgets, status et erreurs sont snapshotés
- `context.Context` est propagé partout
- Timeout, cancellation et shutdown propre arrêtent LLM, tools et tasks enfants
- Les secrets sont redacted avant logs, events, state et admin API

### 8.3 Tool-use loop

- Itération entre le LLM et les tools jusqu'à atteindre le goal
- Les tools exposés au provider viennent exclusivement du `ToolExecutor`
- Les sorties finales typées utilisent `ResultSchema` ; pour Anthropic, le runtime peut exposer un tool interne forcé `ovr_final_result`
- Max iterations configurable (défaut 25)
- Max tokens configurable (défaut 500_000)
- Max coût USD configurable (défaut 5.00)
- Max wallclock configurable (défaut 10 minutes par exécution)
- Si limite atteinte → outcome partiel avec statut `truncated`

### 8.4 ToolExecutor et parallel tool calling

- Tous les tool calls passent par `ToolExecutor`
- Les arguments sont validés contre le JSON Schema avant appel
- Les permissions sont vérifiées avant chaque appel
- Les hooks `BeforeTool` / `AfterTool` sont émis autour de chaque appel
- Les événements `tool_call` / `tool_result` sont enregistrés
- Parallel tool calling est autorisé uniquement pour tools `ReadOnly()` ou `Idempotent(...)`
- Tools sans classification → exécution séquentielle, pas de retry tool automatique
- Cap global et par Pipe pour parallélisme
- Panic, timeout et validation error sont convertis en erreurs structurées

### 8.5 PermissionPolicy

- Défaut sécurisé : deny filesystem hors workspace, env non whitelistée, réseau, process, side effects non déclarés
- Autorisations déclaratives : read file, write file, exec, network host, env var, MCP server, side effect
- Mode dev explicite, jamais équivalent à production
- Décisions auditables dans `EventStream`
- Les admin endpoints, hooks et outils MCP ne bypassent jamais la policy

### 8.6 Sandbox

- Workspace root obligatoire pour `Bash`, fichiers support de Skills, MCP local et tools filesystem
- Résolution realpath pour bloquer path traversal et symlinks hors workspace
- Env minimal + allowlist
- Timeout dur par commande/process
- stdout/stderr bornés
- Réseau off par défaut si enforceable ; sinon fail-fast ou limitation documentée explicitement
- Tests Linux-gated pour garanties OS spécifiques

### 8.7 HookBus et EventStream

Événements minimum :

- `pipeline_started`, `pipeline_completed`, `pipeline_failed`
- `pipe_started`, `pipe_completed`, `pipe_failed`
- `session_started`, `session_saved`, `session_cancelled`
- `llm_call_started`, `llm_call_completed`, `llm_call_failed`
- `tool_call_started`, `tool_call_completed`, `tool_call_failed`
- `permission_decision`
- `schema_validation_passed`, `schema_validation_failed`
- `budget_exceeded`
- `task_started`, `task_completed`, `task_failed`

Hooks minimum :

- `SessionStart`, `SessionEnd`
- `BeforeLLM`, `AfterLLM`
- `BeforeTool`, `AfterTool`
- `SchemaViolation`
- `BudgetExceeded`
- `SubAgentStop`

Les hooks peuvent enrichir, bloquer ou observer selon leur type, mais tout blocage doit produire une erreur structurée et un événement.

### 8.8 StateStore

- Backend mémoire obligatoire pour les tests et l'exécution éphémère
- Backend SQLite embarqué obligatoire en v0.1 pour le mode durable par défaut
- SQLite utilise `modernc.org/sqlite` pour conserver un binaire Go sans `cgo`
- PostgreSQL reste une option future derrière la même abstraction `StateStore`
- Configuration par défaut :
  - `OUVRIER_STATE_BACKEND=sqlite`
  - `OUVRIER_STATE_PATH=.ouvrier/state.db`
- Stocke executions, sessions, snapshots, traces, idempotency keys, schema violations
- Accès concurrent sûr
- TTL / bornes mémoire configurables
- Les endpoints admin et le trace viewer lisent depuis `StateStore`
- La mémoire conversationnelle long-terme et les beliefs persistants restent hors scope v0.1 ; l'historique runtime nécessaire au harnais est dans scope

### 8.9 ResultSchema

- `ovr.Output[T]()` définit le contrat de sortie d'un Pipe
- `ovr.Reply(ovr.JSON[T]())` définit la sérialisation HTTP finale
- JSON Schema généré depuis les types Go et tags `json`
- Validation stricte des outputs
- Tentative de repair LLM optionnelle, bornée par budget, observable
- Violations comptabilisées et consultables dans admin/status/dev viewer

### 8.10 Retry et erreurs

- Erreurs transitoires provider (5xx, network, rate limit) → retry avec backoff exponentiel avant side effects
- Erreurs permanentes provider (4xx, auth, validation) → fail immédiat
- Tools `ReadOnly()` ou `Idempotent(...)` peuvent être retry selon policy
- Tools side-effecting non idempotents ne sont jamais retry automatiquement
- Le journal des tool call IDs empêche la duplication quand une idempotency key existe
- 3 retries provider par défaut, override avec `ovr.Retry(N, backoff)`

### 8.11 Prompt caching

- Hash local de la partie statique du prompt (system + tools + skill + schema) calculé au boot
- Pour Anthropic, le runtime utilise `cache_control` sur les blocs compatibles plutôt qu'une clé propriétaire arbitraire
- Désactivable avec `ovr.NoCache()`

### 8.12 Observabilité

- OpenTelemetry instrumenté à partir de `EventStream`
- Un span par pipeline, Pipe, session, LLM call, tool call, subagent task
- Attributs : tokens input/output, cost USD, latency ms, model, tool name, permission decision, schema status
- Exportable vers Datadog, Grafana, Honeycomb, etc.

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

Le provider Anthropic v0.1 doit supporter :

- Messages API réelle
- tool use avec JSON Schema
- tool choice pour résultat final typé (`ovr_final_result`)
- `cache_control` pour prompt caching
- streaming interne des événements LLM vers `EventStream`
- max tokens par appel
- classification d'erreurs : transient, permanent, rate limit, auth, validation
- usage tokens input/output et coût estimé
- metadata de trace sans secrets

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

Ces métriques sont dérivées de `EventStream` et `StateStore`, pas de compteurs ad hoc séparés.

**Métriques harnais SOTA** :

- sessions démarrées / complétées / annulées
- tool calls autorisés / refusés
- tool calls retry / non retry pour side effects
- sandbox violations
- hook failures
- subagent tasks démarrées / terminées / échouées
- budgets dépassés par type (tokens, coût, wallclock, iterations)

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
- Sessions parent/enfant et SubAgent tasks
- Décisions de permissions et violations de sandbox
- Violations de schéma et repairs

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
- Résolution realpath obligatoire pour bloquer `..` et symlinks hors workspace
- Timeout dur par commande, kill process group au timeout
- stdout/stderr bornés et redacted
- Toute ouverture réseau doit être explicitement autorisée par `PermissionPolicy`
- Si l'isolation demandée n'est pas garantie par la plateforme, le runtime échoue au boot plutôt que de dégrader silencieusement

### 11.5 Permissions et side effects

- Toute action privilégiée passe par `PermissionPolicy`
- Les tools sont classés : `ReadOnly`, `Idempotent`, `SideEffecting`, `RequiresApproval`
- Sans classification, un tool est traité comme side-effecting non idempotent
- Les retries automatiques sont interdits pour side effects non idempotents
- Les décisions de permissions sont auditables mais ne contiennent jamais de secrets
- Les hooks peuvent bloquer une action, mais doivent produire une erreur structurée et un événement

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
- Mémoire long-terme / beliefs persistants au-delà du `StateStore` runtime v0.1
- Mode stateless serverless (Cloudflare Workers, Lambda)
- DSL textuel séparé (`.agent` files)
- Loader YAML alternatif au Go
- WASM target via TinyGo

---

## 15. Critères de succès v0.1

### 15.1 Ordre strict d'implémentation

L'ordre suivant est obligatoire pour éviter d'empiler des stubs autour d'un coeur agentique incomplet :

1. Créer `internal/runtime` et compiler les déclarations en `Plan`, `Trigger`, `Step`, `Terminal`
2. Durcir la validation publique : terminal unique et dernier, compatibilité trigger/sortie, aucun node ignoré
3. Implémenter `ovr.Output[T]()` et le premier contrat `ResultSchema`
4. Introduire `Session`
5. Introduire `EventStream`
6. Introduire l'abstraction `StateStore` avec backends mémoire et SQLite embarqué
7. Implémenter un `ToolExecutor` Go-only réel
8. Prouver la loop avec un provider mock qui appelle un vrai Go tool via `ToolExecutor`
9. Brancher `runtime_http` sur le chemin runtime/harness compilé, pour ne plus retourner `501` sur les cas implémentés
10. Ensuite seulement : provider Anthropic réel, sandbox renforcée, MCP, SubAgent/Task, admin endpoints, CLI avancée, déploiement, streams

Chaque étape doit être couverte par des tests ciblés et laisser `go test ./...` et `go vet ./...` verts avant commit.

### 15.2 Critères de livraison

Le framework est considéré comme livrable v0.1 si :

1. `ouvrier new` génère un projet qui compile et tourne en `ouvrier dev` sans erreur
2. Un pipeline à 3 Pipes (avec Tool, Skill, MCP mock et SubAgent) s'exécute de bout en bout via le harnais SOTA
3. Le binaire `ouvrier deploy ssh` arrive en production avec health check OK
4. Le trace viewer affiche exécutions, sessions, tool calls, permissions, coûts, schémas et SubAgent tasks
5. La documentation utilisateur (le PDF) suffit à un dev junior pour démarrer seul
6. Les deux exemples de référence (Moodle FSRS + tickets triage) tournent
7. Aucun Pipe ne contourne `Session`, `ToolExecutor`, `PermissionPolicy`, `EventStream`, `StateStore` ou `ResultSchema`
8. Les tests sécurité couvrent admin auth, HMAC webhook, sandbox escape, permission deny, redaction secrets et retry sans double side effect

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
- Tests unitaires pour chaque composant du harnais SOTA (`internal/harness`, `internal/tools`, `internal/sandbox`, `internal/policy`, `internal/events`, `internal/state`, `internal/schema`)
- Tests d'intégration pour la CLI (génération de projets test, build, dev)
- Tests mock-provider couvrant la boucle LLM/tool/schema/retry/budget
- Tests sécurité : auth admin, HMAC webhook, sandbox escape, permission deny, redaction secrets
- Tests `go test -race` sur runtime, state store, event stream, tool executor et subagents
- Golden tests sur exemples et documentation pour éviter la dérive API
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
- **Flue framework** (TypeScript) : modèle SOTA de harnais headless, sessions, sandbox, tasks, typed results
- **Claude Code SDK** : hooks, permissions, MCP, subagents, sessions et tool ecosystem comme baseline de production
- **Charm Bracelet** : librairie TUI Go
- **Aguiovanna.fr** : DA et tonalité

---

**Fin des spécifications v0.1**
