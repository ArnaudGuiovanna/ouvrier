# moodle-fsrs

Ouvrier v0.1 reference example for a Moodle-style FSRS scheduler worker.

A single-pipe HTTP worker that accepts a card review at `POST /reviews`,
calls a `compute_fsrs` Go tool, and replies with a typed `Decision` JSON
response: `next_due`, `stability`, `difficulty`, `lapses`.

> The FSRS math in `ComputeFSRS` is a placeholder, not a real spaced
> repetition algorithm. This module exists to demonstrate the Ouvrier
> pipeline shape end to end.

## Layout

```
moodle-fsrs/
  main.go
  go.mod                          (replace -> ../../)
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
curl -X POST http://localhost:8080/reviews \
  -H 'Content-Type: application/json' \
  -d '{
    "card_id": "card-42",
    "rating": 2,
    "history": [
      {"at": "2026-05-01T08:00:00Z", "rating": 1},
      {"at": "2026-05-10T08:00:00Z", "rating": 3}
    ]
  }'
```

The response is JSON matching the `Decision` schema:

```json
{
  "next_due": "2026-05-31",
  "stability": 8.0,
  "difficulty": 3.0,
  "lapses": 0
}
```

## Notes

- `ComputeFSRS` returns plausible numbers; swap it for a real FSRS
  implementation when promoting this example.
- This module uses a `replace` directive that points at the Ouvrier checkout
  one directory above. Adjust it when copying the example out of the repo.
