# ticket-triage

Canonical Ouvrier v0.1 reference example.

A single-pipe HTTP worker that triages a support ticket: it accepts
`POST /tickets/{id}`, loads the ticket through a Go tool, and replies with a
typed `Triage` JSON response containing `priority`, `summary`, and `tags`.

## Layout

```
ticket-triage/
  main.go
  go.mod                          (replace -> ../../)
  ouvrier.worker.json             (integration manifest)
  skills/ticket-triage/SKILL.md   (system-prompt instructions)
```

## Required environment

- `ANTHROPIC_API_KEY` - provider credential for `anthropic/claude-sonnet-4-6`.

Optional:

- `ANTHROPIC_BASE_URL` - override the provider endpoint.
- `PIP_ADMIN_TOKEN`   - protect `/admin/*` endpoints.

## Run

```sh
export ANTHROPIC_API_KEY=...
go run .
```

The worker listens on `:8080`.

## Call

```sh
curl -X POST http://localhost:8080/tickets/T-123 \
  -H 'Content-Type: application/json' \
  -d '{"message":"user cannot sign in"}'
```

The response is JSON matching the `Triage` schema:

```json
{
  "priority": "high",
  "summary": "User cannot sign in to the dashboard after the latest deploy.",
  "tags": ["auth", "dashboard"]
}
```

## Notes

- `LoadTicket` is an in-memory stub; replace it with your own data source.
- The skill body in `skills/ticket-triage/SKILL.md` is injected into the
  system prompt by the harness in declaration order.
- This module uses a `replace` directive that points at the Ouvrier checkout
  one directory above. Adjust it when copying the example out of the repo.
