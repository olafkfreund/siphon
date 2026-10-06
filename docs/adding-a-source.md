# Adding a source (or an action) type

A new source type is **one file plus one switch case in each of three places**.
There is no plugin registry and no interface to implement.

## What a source does

A source turns something into an `Event{Source, ReceivedAt, Headers, Data}`
(`internal/source/http.go`). The pipeline hands that to the rules. Polled
sources implement `Poll(ctx) (Event, error)`; push sources (like webhooks)
serve an `http.Handler` instead (`internal/source/webhook.go`).

Header keys in `Event.Headers` must be lower-case for every source type, because
rules read them as `headers["x-github-event"]`.

## Steps for a polled source

Example: a `file` source that reads a local JSON file.

1. **The source, one file:** `internal/source/file.go`

   ```go
   package source

   import (
   	"context"
   	"os"
   	"time"
   )

   type FileOptions struct {
   	Name string
   	Path string
   }

   type File struct{ Options FileOptions }

   func (s File) Poll(ctx context.Context) (Event, error) {
   	b, err := os.ReadFile(s.Options.Path)
   	if err != nil {
   		return Event{}, err
   	}
   	data, err := DecodeJSON(b) // keeps integers as int64
   	if err != nil {
   		data = map[string]any{"text": string(b)}
   	}
   	return Event{Source: s.Options.Name, ReceivedAt: time.Now(), Data: data}, nil
   }
   ```

   Anything that makes network requests must use the guarded client
   (`guardedClient` in `internal/source/netguard.go`, as `http.go` does) so
   private and metadata addresses stay blocked unless `allow_private` is set.

2. **The config:** in `internal/config/config.go`
   - add the fields it needs to `Source` (with `yaml:"..."` tags; unknown keys
     are rejected, so a field must exist to be used), and
   - add a `case "file":` to the `switch s.Type` in `validateSource`, checking
     required fields. Secret-capable fields should use the `Secret` type so they
     are `env:`/`file:` only and get masked.

3. **The pipeline:** in `internal/job/pipeline.go`, add a `case "file":` to the
   `switch s.Type` in `Pipeline.Poll` that builds the options from the config and
   calls `Poll`, then returns `rule.Event{Source: name, Headers: ev.Headers, Data: ev.Data}`.

4. **The schema:** run
   `UPDATE_SCHEMA=1 go test ./internal/config/ -run TestSchemaUpToDate`
   so `schema/siphon.schema.json` matches the new fields (the test fails when it
   is stale).

5. **A test** next to the source (`internal/source/file_test.go`) and a
   validation case in `internal/config/config_test.go`.

`serve` and `run-once` need no changes: they poll every non-webhook source
through `Pipeline.Poll`.

## Adding a push source

Follow `internal/source/webhook.go`: build an `http.Handler`, return it from
`Pipeline.Webhooks()` (`internal/job/hooks.go`), and it is mounted at
`POST /hook/{source}` without portal auth (the handler must authenticate the
caller itself, e.g. with an HMAC). Feed accepted events to `Pipeline.deliver`
so the replay record and the enqueue commit together.

## Adding an action type

Same shape: one file in `internal/action/` (see `cmd.go` and `unit.go`), a field
on `config.Action` plus a check in `validateAction`, and one case in the
`switch` in `Pipeline.run` (`internal/job/pipeline.go`). If it should work as a
routine step, add the case in `execOnce` (`internal/job/routine.go`) and the
field on `config.Step`. Anything that runs external code must go through the
sandbox options and secret masking that `RunCmd` uses.
