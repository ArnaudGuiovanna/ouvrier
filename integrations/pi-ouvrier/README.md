# Extension Pi Ouvrier

Extension Pi qui transforme les workers Ouvrier en flotte asynchrone autour de
la session de coding agent synchrone de Pi.

- Pi reste le cockpit de développement au premier plan.
- Les workers Ouvrier restent des projets Go normaux : trigger → harness → outcome.
- Les retours des workers arrivent dans une Inbox Ouvrier au lieu d'interrompre
  le flux de travail actif.

## Installation globale

Pi découvre automatiquement les packages déclarés dans `~/.pi/agent/settings.json`.
Installation globale recommandée :

```sh
pi install ~/.pi/agent/packages/ouvrier-pi-extension
```

Après installation, recharge Pi avec `/reload` ou redémarre Pi. L'extension
découvre les workers en scannant le workspace courant pour trouver des manifests
`ouvrier.worker.json`.

## Qualité

Cette intégration TypeScript est un prototype d'intégration et n'est pas
couverte par la CI Go principale du dépôt. Après modification, installe les
dépendances de ce package puis lance :

```sh
npm run typecheck
```

Pour un test temporaire uniquement, tu peux aussi lancer :

```sh
pi -e ./integrations/pi-ouvrier/src/index.ts
```

## Commandes

```txt
/ovr workers                         lister les workers découverts
/ovr inbox                           ouvrir la messagerie des workers
/ovr trigger <worker> [event] [json] déclencher un worker via /admin/trigger
/ovr trace <exec_id>                 ouvrir la trace d'une exécution
/ovr health                          rafraîchir /admin/health et les capacités
/ovr compose [objectif]              demander à Pi de créer un worker Ouvrier
/ovr read-all                        marquer les messages comme lus
```

Raccourci :

```txt
Ctrl+Shift+O                         ouvrir l'Inbox Ouvrier
```

Outils exposés à Pi :

- `ouvrier_workers`
- `ouvrier_trigger`
- `ouvrier_inbox`

Skill fournie :

- `/skill:ouvrier-worker` pour créer/corriger un worker avec les bonnes sources de vérité (`ouvrier` installé et manifest `ouvrier.worker.json`).

Les commandes produisent aussi une sortie texte en mode non interactif (`pi -p`) au lieu de dépendre uniquement des panneaux TUI.

## Manifest worker

Manifest minimal :

```json
{
  "name": "codebase-reviewer",
  "description": "Relit en arrière-plan la cohérence globale du code.",
  "events": ["pi.agent_end"],
  "outcomes": ["feedback"],
  "admin_url": "http://127.0.0.1:8080"
}
```

Surcharge optionnelle de la variable d'environnement du token :

```json
{
  "admin_token_env": "CODEBASE_REVIEWER_ADMIN_TOKEN"
}
```

Sinon l'extension utilise `OUVRIER_ADMIN_TOKEN` ou `PIP_ADMIN_TOKEN`.

## API runtime utilisée

- `GET /admin/health`
- `GET /admin/capabilities`
- `GET /admin/events?format=sse`
- `POST /admin/trigger`
- `GET /admin/traces/<exec_id>`

## Pont d'événements

L'extension peut envoyer des événements de cycle de vie Pi aux workers qui s'y
abonnent dans le champ `events` du manifest :

- `pi.agent_end`
- `pi.turn_end`
- `pi.tool_execution_end`

Un worker qui déclare `pi.*` ou `*` reçoit tous les événements du pont.

Le body envoyé à `/admin/trigger` est une enveloppe :

```json
{
  "event": "pi.agent_end",
  "source": "pi",
  "worker": "codebase-reviewer",
  "at": "2026-05-29T00:00:00Z",
  "body": {
    "event": "pi.agent_end",
    "source": "pi",
    "workspace": "/path/to/workspace",
    "payload": {}
  }
}
```

L'extension choisit le premier plan compilé depuis `/admin/capabilities`, sauf
si une chaîne d'événement nomme explicitement un trigger Ouvrier concret comme
`http.POST /review`, `cron @every 1h`, `stream nats://...` ou `webhook github`.

## Messages d'Inbox

`/admin/events` est streamé en mode SSE. L'Inbox affiche :

- `sink_logged` comme retour de worker ;
- les échecs comme `pipeline_failed`, `tool_call_failed`, `budget_exceeded` ;
- les demandes d'approbation et les problèmes de schéma ou de hook.

Si un payload `sink_logged` contient un JSON avec `title`, `body`/`message`,
`severity` ou `actions`, ces champs sont utilisés directement dans l'interface
de messagerie.
