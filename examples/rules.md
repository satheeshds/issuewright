# Repo rules for automated changes

These are read by issuewright and prepended to every prompt. Keep them short:
they are instructions, not documentation.

## Shape of a change

- Schema first: `packages/database/schema.sql` and the SQLC queries change
  before the Go code that uses them, then `make sqlc`.
- Never hand-edit generated output (`apps/api/db/*.go`, `packages/openapi/**`).
  Regenerate with `make sqlc|openapi|ts|openapi-dart` and commit the result.
- Handler signatures or payloads that move the contract need Swag annotations,
  then `make openapi`, `make ts`, `make openapi-dart` in the same commit.

## Verification

- Go: `cd services/api && go test ./... && go build ./...`
- Web: `cd web && npm ci && npm run build && npm test`
- Mobile: `cd mobile && flutter pub get && flutter analyze && flutter test`

Run the narrowest set that covers the change; the runner runs the rest.

## Never

- Run migrations or anything that writes to a database.
- Touch `.env` files, deployment scripts, or CI workflows.
- Commit a commented-out block, a TODO without an issue number, or a debug print.
