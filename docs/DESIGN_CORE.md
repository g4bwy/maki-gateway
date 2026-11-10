# DESIGN_CORE: implementation spec for internal/registry, internal/httpapi, cmd/gateway

Produced by a design pass over docs/CONTRACTS.md (binding) and internal/model/types.go.
Implement faithfully; deviations must be noted in the final report. Module `maki-gateway`,
Go 1.27, stdlib only, gofmt clean.

## Constructor truth (other crews build these concurrently; wire main.go to EXACTLY this)
- `acpengine.New(makiPath string, hub *model.Hub) *acpengine.Engine` (Kind model.KindWeb)
- `attach.New(sockDir string, hub *model.Hub) *attach.Engine` (Kind model.KindLive)
- `store.New(stateDir string) (*store.Engine, error)` (Kind model.KindStored; missing dir = empty list, no error)

## internal/registry (package registry)
Consts `PollInterval = 5s`, `Debounce = 250ms`, `EventTypeSessions = "sessions"`.
- `SessionKind(s string) string`: "acp"|"web"→web, "attach"|"live"→live, "store"|"stored"→stored.
- `Merge(engines ...model.Engine) []model.Session`: flatten List() rows (skip errored engines and
  empty IDs); rank live 3 > web 2 > stored 1; first row per ID wins; later rows fill only blank
  fields (Title/Status/Cwd/Model/UpdatedAt/PluginPID). Final sort UpdatedAt desc, ID asc tiebreak
  (slices.SortStableFunc).
- `New(hub *model.Hub, engines ...model.Engine) *Registry` mapping byKind and byName (both
  spellings accepted). `Start(ctx)` = one synchronous refresh WITHOUT publish (seed `last`), then
  poll goroutine behind sync.Once.
- `poll(ctx)`: select over ctx.Done(), r.signal (buffered 1, trailing-edge debounce armed once,
  cleared on fire), 5s ticker (cancels pending debounce, merges immediately), and hub.Subscribe()
  events filtered by `refreshOn`; defer Unsubscribe. `Refresh()` non-blocking signal.
  `Sessions()` returns slices.Clone. `Find(id)`. `EngineFor(id)` = owner of winning row, fallback
  acp→attach→store. `EngineForKind`, `EngineByName`.
- `refresh(publish bool)`: merge, json.Marshal, compare to last under RWMutex; publish
  Event{Type:"sessions"} only on change and only when publish.
- `refreshOn(e)`: true for status|dirty|exit; for update|notice true when Reason or raw e.ACP
  contains "turn_end".

## internal/httpapi (package httpapi)
`Config{Hub *model.Hub; Registry *registry.Registry; MakiPath, Addr, Version string; LookupEnv
func(string) string; Static http.Handler; Engines []model.Engine; Ctx context.Context}`;
`New(Config) (*Server, error)`; `(*Server).ServeHTTP`.
- Auth: `newAuth(addr, getenv)`; host loopback = "localhost" or net.ParseIP(host).IsLoopback()
  after stripping %zone; EMPTY host (bare ":8765") counts as PUBLIC. Public + empty
  MAKI_GATEWAY_TOKEN = New() error (refuse bind). Per-request accept `Authorization: Bearer <tok>`
  or `?token=`, subtle.ConstantTimeCompare, else 401. Covers static and SSE too.
- Mux (Go 1.22 wildcards): GET /api/health · GET|POST /api/sessions · DELETE /api/sessions/{id} ·
  GET /api/sessions/{id}/messages · POST /api/sessions/{id}/prompt|cancel|mode|title ·
  POST /api/permissions/{engine}/{request_id} · GET /api/plan/{id} · GET /api/search ·
  GET /api/analytics · GET /api/events. Wrong method = 405 free via mux.
- ServeHTTP: auth, then logged wrapper (status capture, Flush forwarded, Unwrap for
  ResponseController), slog.Info(method, path, status).
- Health: {version (Config.Version default "dev"), maki (exec `maki --version` once under
  sync.Once, trimmed, "" on failure), sessions (registry count)}. Export MakiVersion() for main warmup.
- Messages: ?last default 200. Source by KIND of merged row: web→"acp", live→"attach",
  stored|unknown→"file" via store engine (deleted file 404). Emit `[]` never null.
- SSE: text/event-stream, no-cache, literal `retry: 3000`, per hub event
  `event: maki\ndata: <Event JSON>\n\n` flush each; `: ping\n\n` every 20s; return on
  r.Context().Done() or Config.Ctx.Done(); defer Unsubscribe.
- Errors via errors.Is: ErrBusy→409 {"error":"busy"} · ErrOpenElsewhere→409 {"error":"open
  elsewhere"} · ErrUnsupported→501 · ErrNotFound→404 · else 500. Helpers writeErr/writeJSON.
- `StaticHandler(dir string)`: "/" serves dir/index.html (ServeFile), rest http.FileServer.

### Resolved ambiguities (implement exactly)
1. Mutation routing by merged row KIND, never EngineFor: web→acp engine, live→attach. Unknown id
   404. Stored: prompt/cancel 404; mode/title 405.
2. /api/plan, /api/search, /api/analytics are NOT on model.Engine. Probe optional interfaces
   locally: `planner{Plan(id string) (json.RawMessage, error)}`, `searcher{Search(q string, limit
   int) ([]SearchHit, error)}` with SearchHit{Session,Title,Snippet string}, `analyst{Analytics(days
   int) (json.RawMessage, error)}`. Fallback if absent: search scans Messages(id,200) of merged
   sessions case-insensitive substring, cap 50, snippet from type:"text" blocks; analytics returns
   per_session from registry rows with zero cost and daily []. plan on web kind = 501 per contract.
3. Cancel on live kind: return 501 directly (do not trust engine error text).
4. First merge publishes nothing (seed without event).
5. Merge updated_at: winner's value unless zero, then runner-up (NOT max-across-rows).
6. Permissions: match {engine} against engine name then session kind; ParseInt request_id, bad →
   400; RespondPermission ErrNotFound → 404 (contract "expired").
7. Health "version" = gateway Config.Version, distinct from cached maki --version string.

## cmd/gateway/main.go
Flags: -maki "maki" · -addr "127.0.0.1:8765" · -state (default $XDG_STATE_HOME/maki else
~/.local/state/maki) · -web "web" · -sockdir (default <state>/gateway). slog text → stderr.
model.NewHub() → engines (store error tolerated: nil→skip store rows) → registry.New(hub, acp,
attach, store) → reg.Start(ctx) → httpapi.New(cfg) (error = refuse to start) → srv.MakiVersion()
warm → http.Server{Handler, ReadHeaderTimeout: 10s} goroutine → signal.NotifyContext(os.Interrupt,
SIGTERM) → Shutdown(3s) then Close. No child-kill: acp children die on stdin close.

## Tests (fakes in _test.go only)
- registry_test: fake engine, settable rows. Live>stored precedence; blank fields filled from
  runner-up; one row per id across three engines; sort order; EngineFor owner + fallback; change
  detection: refresh(false) seed, real Hub subscribe, refresh(true) unchanged → no event, mutate
  → exactly one "sessions" event, unchanged again → none.
- httpapi tests: fakeEngine implementing all 11 model.Engine methods + capability variant with
  planner/searcher/analyst; httptest over real registry. Cover: every route, 409 busy, 409 open
  elsewhere, 404 stored prompt, 405 stored mode, 400 delete live, 501 cancel live, source per
  kind, last default/override, permission routing + 404 expired, search cap 50, SSE line scan in
  goroutine with 2s timeout, auth table over ":8765" "127.0.0.1:8765" "localhost:8765"
  "[::1]:8765" "0.0.0.0:8765" "10.0.0.5:8765" (public ones need env token to construct).

Verified ground truth from the design pass: session jsonl record tags are
header|msg|out|sub_msg|frame|meta (maki/maki-storage/src/sessions.rs:759-776); only t=="msg"
lines are transcript; meta is the analytics source.
