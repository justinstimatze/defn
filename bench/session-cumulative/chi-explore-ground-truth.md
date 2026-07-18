# chi-explore.txt — ground-truth answer key

*Verified against chi source at scratchpad/delta-bench/chi (v5.x). 2026-07-17.*

Each turn's expected key facts. Rubric: an answer scores 1.0 if it
mentions all key facts, 0.5 if it mentions half, 0.0 if wrong or
missing the core mechanism.

---

## Turn 1 — chi.Mux routing flow (ServeHTTP top-level)

**Source:** mux.go:63-92 (`ServeHTTP`), mux.go:414 + mux.go:494 (`mx.handler` build).

**Key facts:**
- ServeHTTP checks `mx.handler == nil` → calls `NotFoundHandler` if so.
- Checks for existing `Context` at `RouteCtxKey` (parent-router case) → dispatches without pool.
- Otherwise: pulls a `Context` from `mx.pool` (sync.Pool), assigns `Routes = mx`, captures `parentCtx`.
- Wraps request context with `RouteCtxKey → rctx` via `r.WithContext(context.WithValue(...))`.
- Calls `mx.handler.ServeHTTP(w, r)` — where `mx.handler = chain(mx.middlewares, http.HandlerFunc(mx.routeHTTP))` (built lazily via `updateRouteHandler`).
- Puts context back in pool after serving.

**Also acceptable:** noting the sync.Pool allocation-savings motivation; `RouteContext` accessor is what handlers use downstream.

---

## Turn 2 — Direct callers of `routeHTTP`

**Source:** grep `routeHTTP` in *.go (non-test).

**Key facts:**
- **No direct function callers.** `routeHTTP` is assigned as a value to `mx.handler` (via `http.HandlerFunc(mx.routeHTTP)`) at TWO sites:
  - mux.go:414 in `Mux.Handle` inline branch
  - mux.go:494 in `updateRouteHandler` (wrapped in `chain(mx.middlewares, ...)`)
- Test callers: none direct (only via `mx.handler.ServeHTTP` in mux_test).
- Answer must distinguish between "called directly by name" (0) and "referenced as method value" (2 sites).

**Rubric:** 1.0 if identifies the 2 assignment sites AND explains the "assigned as HandlerFunc, called via mx.handler" mechanism. 0.5 if only says "invoked via mx.handler". 0.0 if names wrong functions.

---

## Turn 3 — Mux vs Router relationship

**Source:** chi.go:67 (`type Router interface`), mux.go:21 (`type Mux struct`), chi.go:61 (`NewRouter() *Mux`).

**Key facts:**
- `Router` is an INTERFACE defined at chi.go:67.
- `Mux` is a STRUCT at mux.go:21 that IMPLEMENTS `Router` (satisfies all methods).
- `NewRouter()` returns `*Mux` — comment says "returns a new Mux object that implements the Router interface".
- Router embeds `http.Handler` and `Routes` interface.
- Router lists ~24 methods: Use, With, Group, Route, Mount, Handle, HandleFunc, Method, MethodFunc, Connect-Trace (9 verb methods), NotFound, MethodNotAllowed.

**Rubric:** 1.0 requires "Router is interface, Mux implements it". 0.5 if hedges. 0.0 if says Mux is interface or the relation is embedding.

---

## Turn 4 — Mux.Handle callers, blast radius

**Source:** grep `.Handle(` in *.go.

**Key facts:**
- **Zero production callers of `Mux.Handle` inside chi package.** All calls are internal method calls (`mx.handle(...)` lowercase) or external users.
- Test callers: mux_test.go:273 (`mux.Handle("/api*", apiRouter)`), mux_test.go:699 (`r1.Handle(tc.pattern, ...)`) — active tests.
- path_value_test.go:52 uses it in a table-driven test.
- Signature changes would break: (a) chi's own tests as listed, (b) all external users (Router interface method).
- Note: Handle is part of the `Router` interface (chi.go:89) — changing the signature breaks the interface contract too.

**Rubric:** 1.0 if identifies zero prod callers + the interface-contract implication. 0.5 if only names tests. 0.0 if invents callers.

---

## Turn 5 — chi.Middlewares

**Source:** chi.go:134 (`type Middlewares []func(http.Handler) http.Handler`), chain.go:5 (`Chain`).

**Key facts:**
- `Middlewares` is a slice type: `[]func(http.Handler) http.Handler`.
- Used as return type of `Chain(...)` — bundles a variadic middleware slice.
- Has methods `Handler(h http.Handler) http.Handler` and `HandlerFunc(h http.HandlerFunc) http.Handler` — both build a `ChainHandler`.
- Also returned by `Routes` interface method `Middlewares() Middlewares` (chi.go:128 area).
- Pattern enforced for users: middleware must have signature `func(http.Handler) http.Handler` — one-arg, one-return-arg. Standard net/http middleware convention.

**Rubric:** 1.0 requires type signature + Chain relationship. 0.5 if only signature. 0.0 if wrong signature.

---

## Turn 6 — chi.Chain construction + return

**Source:** chain.go:5-8 (`Chain`).

**Key facts:**
- Signature: `func Chain(middlewares ...func(http.Handler) http.Handler) Middlewares`.
- Body is one line: `return Middlewares(middlewares)`.
- Type conversion: the variadic `[]func(...)` slice is CAST to `Middlewares` (which is defined as that same slice type).
- Returned value is a `Middlewares` — the caller then uses `.Handler(h)` or `.HandlerFunc(h)` to compose with an endpoint.

**Rubric:** 1.0 requires "Middlewares type conversion" + return. 0.5 if only mentions "returns Middlewares". 0.0 if invents wrapper struct.

---

## Turn 7 — Nested sub-router request path

**Source:** mux.go ServeHTTP → mx.handler → routeHTTP → tree.FindRoute → mountHandler → sub-Mux ServeHTTP.

**Key facts (file touches):**
- mux.go — parent Mux.ServeHTTP, mx.handler chain, routeHTTP.
- tree.go — FindRoute lookup.
- mux.go — mountHandler (defined in Mount at mux.go:315-329).
- context.go — RouteContext/Context accessors mid-flight.
- mux.go — sub-Mux.ServeHTTP (same code, re-entered).
- Sub-flow adjusts `rctx.RoutePath = mx.nextRoutePath(rctx)` before delegating.

**Rubric:** 1.0 if identifies re-entrant ServeHTTP + tree.go lookup + RoutePath shift. 0.5 if names 2 of 3 files. 0.0 if says handler is called directly without re-entry.

---

## Turn 8 — Middleware using context values

**Source:** grep `context.WithValue|contextKey|CtxKey` in middleware/*.go (non-test).

**Key facts (4 middlewares store keys):**
- `logger.go` — `LogEntryCtxKey` (`*contextKey{"LogEntry"}`)
- `request_id.go` — `RequestIDKey` (`ctxKeyRequestID int` = 0)
- `url_format.go` — `URLFormatCtxKey` (`*contextKey{"URLFormat"}`)
- `value.go` — user-provided arbitrary `key interface{}` (via `WithValue(key, val)`)

Plus `middleware.go` — declares the private `contextKey` struct type used by others (not a middleware itself).

**Rubric:** 1.0 requires 4 correct names + key types. 0.5 if 2-3. 0.0 if wrong names or misses that `value.go` uses arbitrary keys.

---

## Turn 9 — Middleware inheritance in Mux.Route

**Source:** mux.go:278-286 (`Route`) → mux.go:295+ (`Mount`).

**Key facts:**
- `Route` calls `NewRouter()` → returns a FRESH `*Mux` with an EMPTY middleware slice.
- The sub-router does NOT copy the parent's middlewares.
- Sub-router only inherits: `notFoundHandler` and `methodNotAllowedHandler` (Mount lines 308-313), IF the sub has none set.
- **Mechanism for middleware "inheritance":** parent's middleware still fires because parent's `mx.handler = chain(mx.middlewares, mx.routeHTTP)` wraps ALL routes including the mount handler. The sub-router's own middlewares (if any) apply on top of the parent chain when the mount handler dispatches into the sub.
- So it's chain-through-parent, NOT copy-inheritance.

**Rubric:** 1.0 requires "fresh middleware slice, parent chain wraps outermost". 0.5 if says "inherited" without mechanism. 0.0 if claims explicit copy or inheritance list.

---

## Turn 10 — Extension points chi exposes

**Source:** chi.go Router interface, middleware/*, tree/context accessors.

**Key facts (chi's public extension surfaces):**
1. **`Middlewares` / `Chain`** — anyone can write `func(http.Handler) http.Handler` and plug in via `Use` / `With`.
2. **Router interface** — external packages can implement Router (docgen relies on this).
3. **`Routes` interface** — traversal for external tools (docgen).
4. **Sub-routing** — `Route()` / `Mount()` accept arbitrary `http.Handler` including 3rd-party routers.
5. **NotFound / MethodNotAllowed** handlers — users override via `NotFound()` / `MethodNotAllowed()`.
6. **Context accessors** — `RouteContext(ctx)`, `URLParam(r, key)` — users can build helpers around them.

**Rubric:** 1.0 requires ≥3 of the extension points named accurately. 0.5 if 2. 0.0 if generic hand-waving.

---

## Re-projection with verified answers

The turns are answerable from chi source alone — no external context
needed. Estimated tool calls per arm (files vs defn-today vs
expand-v1):

- Files arm: **21-26** (mostly Read + grep, some multi-file).
- Defn-today arm: **26-34** (read → impact → read chains on turns 2/4/5).
- Expand-v1 arm: **17-23** (expand collapses turns 2/4/5, still needs
  multi-read on turns 1/3/6/7/9 that want file-siblings — v2).

Delta vs turns.txt projection: same ballpark, but the WORKLOAD MATCHES
expand's mechanism. Turns 2, 4, 5 alone save 3-6 tool calls in a
10-turn session — that's the compounding cache-read win.

## Firing recommendation

Yes, this is the right bench to fire paid runs on. Estimated cost:
$10-20 for one comparison arm. Ground truth verified. Rubric written.

Suggested arm order:
1. `files` (baseline). ~$10.
2. `defn-natural` (expand available, not forced). ~$10-15. Tests
   whether the model reaches for expand without preamble prescription.
3. `defn-expand-forced` (with `expand-preamble.md`). ~$10-15.

Total: **$30-40** for all three arms.

If (2) shows the model doesn't use expand naturally, (3) is the
"can it help when told to" test. If (2) already wins, we ship.

Author expected: justin (the human). Bench harness is
[`run-arm.sh`](./run-arm.sh) — same invocation, different turns file.
