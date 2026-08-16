# Backfill

Memories that predate sync do not upload on their own. `cmemlan backfill` fixes that.

## Why they are stuck

claude-mem's schema migration **v47** ran a one-time cutoff when cloud sync shipped: it recorded every
locally authored row in `sync_launch_exclusions` and stamped `synced_at` on all of them. Those rows
are therefore marked as already uploaded despite never having gone anywhere, and the flusher — which
only ever looks for `synced_at IS NULL AND origin_device_id IS NULL` — skips them forever.

Backfill clears that stamp so the worker treats them as new.

## Usage

```bash
cmemlan backfill --dry-run          # counts and bytes, changes nothing
cmemlan backfill                    # queue everything local
cmemlan backfill --project my-app   # just one project
cmemlan backfill --undo             # put it back
```

Always start with `--dry-run`. It reports how many rows and roughly how much content would upload, so
you know what you are about to put on the hub.

## What it does, precisely

Inside a single transaction, per replicating table:

1. Copy the rows of `sync_launch_exclusions` this run consumes into a table cmemlan owns, so `--undo`
   can restore them.
2. Delete exactly those baseline rows — scoped by the same predicate as the update below, so a
   `--project` run cannot destroy another project's baseline.
3. `UPDATE … SET synced_at = NULL WHERE origin_device_id IS NULL AND <scope>`.

Before any of that, it takes a `VACUUM INTO` snapshot of the whole database — a consistent copy that
works against a live writer, unlike copying the file — and prints the path.

## Safeguards

- **A backup first**, always, with the restore path printed.
- **Refuses to run while claude-mem's worker is writing** unless you pass `--force`. Modifying a
  database underneath a running worker is not something to do casually.
- **Refuses on an unrecognised schema.** If migration v47 has not been applied, this database predates
  the cutoff and there is nothing to reverse.
- **Honours `CLAUDE_MEM_EXCLUDED_PROJECTS`.** If you excluded a project from capture, its history is
  not uploaded behind your back.
- **`origin_device_id IS NULL` only.** Rows that arrived from the hub are never requeued, so a
  backfill cannot push another device's memories back as if they were yours.
- **Idempotent.** Running it twice costs bandwidth, not correctness: the hub dedupes on
  `(entity_id, entity_rev)`.

## Undo

```bash
cmemlan backfill --undo
```

restores the saved baseline rows and re-stamps the affected memories. If something went badly wrong,
the `VACUUM INTO` snapshot is a complete database — stop the worker and copy it back over
`claude-mem.db`.

## Caveats

- Backfill queues rows; the worker uploads them in the background. Watch progress with
  `cmemlan status`.
- A large backfill can push a lot of content at once. Check the byte estimate from `--dry-run` first.
- The other device only receives them once it pulls. With no active Claude Code session it polls
  every five minutes; restarting it triggers an immediate catch-up.
- This depends on claude-mem's internal schema. It asserts what it expects and refuses when it does
  not recognise the database, but a future claude-mem release could still change the mechanics —
  `doctor` warns when your version is outside the tested range.
