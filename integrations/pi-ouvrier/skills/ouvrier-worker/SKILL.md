---
name: ouvrier-worker
description: Concevoir, créer ou corriger un worker Ouvrier Go normal et éditable, avec ouvrier.worker.json, en gardant Pi au premier plan synchrone et Ouvrier en middleware asynchrone.
---

# Ouvrier Worker

Utilise cette skill dès que l'utilisateur demande de créer, modifier ou diagnostiquer un worker Ouvrier.

## Sources de vérité

1. Utilise d'abord le binaire installé :
   ```sh
   which ouvrier
   ouvrier --help
   ouvrier new --help
   ouvrier show --help
   go version -m "$(which ouvrier)"
   ```
2. Si du code source de l'API Ouvrier est nécessaire, utilise la source correspondant au binaire installé quand elle existe, par exemple `/tmp/ouvrier-framework-install/ouvrier` dans ce workspace.
3. N'utilise pas `/home/ubuntu/ouvrier` comme source de vérité API sauf si l'utilisateur demande explicitement de travailler sur le framework lui-même : ce dossier est le checkout de développement du framework.

## Règles d'architecture

- Pi reste l'agent de coding synchrone au premier plan.
- Ouvrier reste un middleware d'arrière-plan : le worker est un projet Go normal, éditable, testable.
- Chaque worker doit inclure un `ouvrier.worker.json` exploitable par l'extension Pi/Ouvrier.
- Pour un worker HTTP asynchrone, préfère `ovr.Reply(ovr.Accepted())` et conserve les résultats dans les traces/admin events.
- Déclare un `WorkerPool` raisonnable pour éviter de saturer le modèle.
- Si le modèle ne contient pas de préfixe provider, choisis le provider Ouvrier explicite approprié et documente-le. Exemple : `qwen3-coder:480b-cloud` via Ollama devient `ollama/qwen3-coder:480b-cloud`.

## Checklist de création

1. Scaffold si possible avec `ouvrier new --yes ...`, puis ajuste le projet au besoin.
2. Vérifie que `go.mod` pointe vers l'API installée ou vers un module publié, pas vers le checkout de dev par accident.
3. Ajoute/valide `ouvrier.worker.json` :
   ```json
   {
     "name": "nom-du-worker",
     "description": "...",
     "events": ["http.POST /route"],
     "outcomes": ["accepted_reply", "trace"],
     "admin_url": "http://127.0.0.1:PORT"
   }
   ```
4. Assure la cohérence entre `PIP_PORT`, `admin_url` et l'adresse passée à `ovr.Run`.
5. Lance `gofmt -w` et `go test ./...`.
6. Si possible, démarre brièvement le worker et vérifie `/admin/health` puis `/admin/capabilities`.
7. Ne laisse pas le worker en arrière-plan après les tests, et supprime l'état runtime `.ouvrier/` si tu l'as créé pendant les validations.

## Commandes de vérification utiles

```sh
cd /chemin/du/worker
go test ./...
PIP_ENV=dev PIP_PORT=8090 go run .
curl -fsS http://127.0.0.1:8090/admin/health
curl -fsS http://127.0.0.1:8090/admin/capabilities
```
