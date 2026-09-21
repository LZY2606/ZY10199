# Name Resolution

This document is an **executable** map of how Jet binds names. Every rule below
is pinned to a function and asserted by the characterization tests in
`name_resolution_*_test.go`, which render templates with an internal observer
attached and check both the rendered output and the decisive resolution path.

There are **two namespaces** and **two phases**:

- **Variables** — identifiers (`x`), the context (`.`), Set globals and built-in
  functions. Resolved at **run time** by walking a lexical scope chain.
- **Blocks ("macros")** — named `{{block name(...)}}...{{end}}` definitions,
  invoked with `{{yield name(...)}}`. Bound mostly at **parse time** into a
  per-template block table, then selected at **run time** by walking block
  tables attached to the scope chain.

## Parse-time vs run-time binding by syntax

| Syntax | Parse-time binding | Run-time behaviour |
|---|---|---|
| `{{extends "p"}}` | Resolves `p` relative to the current template (`Set.getSiblingTemplate`); parent is parsed on demand. After parsing, the parent's `processedBlocks` are merged into the child first (lowest precedence). | The parent template (not the child) is executed; content in the child outside `block` definitions is discarded. `Template.Execute`/`executeInclude` walk `t.extends` to the root. |
| `{{import "p"}}` | Resolves `p` like extends, appends to `Template.imports`; each imported template's `processedBlocks` are merged **in statement order** (a later import wins a name collision). | Imported templates are never executed; only their block lists become available to `yield`. Variables inside an imported template never exist. |
| `{{include "p" [ctx]}}` | Only an `IncludeNode` is produced (`parseInclude`); the name expression is not resolved yet. | `Runtime.executeInclude` evaluates the name (so it can be a variable/expression), loads via `Set.getSiblingTemplate`, opens a new lexical scope, swaps in the included template's block table, optionally switches `.` to `ctx`, and runs the extends-resolved root. Bounded to 100 000 nested includes. |
| `{{block name(args)}}...{{end}}` | Recorded in `Template.passedBlocks` (`parseBlock`) and merged **last** (highest precedence). Also emits a `BlockNode`. | Executed immediately where defined (unless it lives in an imported/extended template that is not itself run). `Runtime.executeList` selects the winning block via `Runtime.resolveBlock`, then calls `executeYieldBlock`. |
| `{{yield name(args) [ctx] [content]}}...{{end}}` | Produces a `YieldNode` (`parseYield`); binds nothing. | Looks `name` up via `Runtime.resolveBlock`, binds named/default arguments into a fresh scope, optionally switches `.` to `ctx`, and runs the block list (`executeYieldBlock`). |
| `{{yield content [ctx]}}` | Marks a content slot inside a block. | Invokes the closure captured by the enclosing `yield ... content`; the closure restores the yield site's scope. |
| `{{ x := v }}` | Produces a `SetNode` with `Let=true` (`assignmentOrExpression`). | `executeLetList` creates the binding in the **current** scope (`Runtime.Let`), shadowing same-named bindings in parent scopes. |
| `{{ x = v }}` | `SetNode` with `Let=false`. | `executeSetList` → `Runtime.setValue` walks current→parent scopes and mutates the **first existing** binding; assigning an uninitialised name errors. |
| `{{ x }}` / `{{ f(a) }}` | `IdentifierNode` / `CallExprNode`. | `Runtime.resolve` (below) at run time. |
| `{{ .field }}` | `FieldNode` (the leading `.` is the context). | Resolved against the current `.` (`Runtime.context`), then by field/index resolution — never against variable scopes. |

## Block ("macro") precedence

After `Set.parse` finishes, each template has one `processedBlocks` table.
The merge in `Set.parse` (via `Template.bindBlocks`) is, in order:

1. **extends** — the extended template's table (lowest precedence).
2. **imports** — each imported template's table, in the textual order of the
   `import` statements; the **later** import wins on a name collision.
3. **self** — blocks defined in the template itself (`passedBlocks`), highest
   precedence; this is how a child overrides a parent's default block.

> The only ordering contract here is the explicit merge order above. The merge
> iterates Go maps, and **map traversal order is intentionally not part of the
> contract**; two *different* block names never interact regardless of order.

Subtlety observed for `extends`: when the parent is parsed lazily the pending
child is already associated with the parse, so the child's overrides are present
in the shared table before inheritance copies it. Net effect is simple:
**child `self` block wins, parent default survives when not overridden.**

## Run-time variable resolution order

`Runtime.resolve(name)` probes, in exactly this order
(see `resolveObserved` for the instrumented copy):

1. `name == "."` → current context (`Runtime.context`).
2. Lexical variable scopes, innermost first:
   `scope.variables` on the current scope, then each `parent`.
   - depth 0 is the **execute scope**, seeded from `Template.Execute`'s
     `variables` argument;
   - deeper scopes come from block/yield parameters, `range`/`if` `:=`
     declarations, `try/catch` error bindings and `include`.
3. **Set globals** (`Set.AddGlobal`, guarded by `Set.gmx`).
4. **Default built-ins** (`defaultVariables` in `default.go`: `upper`, `len`,
   `map`, `raw`, `includeIfExists`, `exec`, …).
5. Otherwise: `identifier %q not available in current (...) or parent scope,
   global, or default variables`.

Assignment rules (mirroring the read order):

- `Let` (`:=`) writes the current scope unconditionally — shadowing is allowed.
- `Set` (`=`) walks the chain and overwrites the nearest existing binding; it is
  an error to assign a name that does not exist (`Runtime.setValue`).
- `LetGlobal` walks to the top-most template scope (not the Set globals map).

## Run-time block resolution

`Runtime.resolveBlock(name)` uses `(*scope).getBlock`: it reads `scope.blocks`
on the current scope then each parent. Each scope's `blocks` is the
`processedBlocks` table active in that frame:

- `Template.Execute` seeds it with the executed template's table;
- `executeInclude` swaps in the included template's table for the included
  scope, so a `yield` inside an include resolves the included template's macros;
- `newScope` carries the current table into child scopes.

Only the named slot of each table is read, so the walk is independent of map
iteration order.

## Edge cases and guarantees

- **Circular `extends`/`import`**: resolution happens during parsing and there
  is no parse-time cycle guard, so a cycle recurses until the Go runtime aborts
  with a fatal stack overflow (unrecoverable; the process exits). Characterized
  in a subprocess test.
- **Circular `include`**: run-time recursion is bounded; at depth 100 000
  `executeInclude` returns `maximum 'include' depth (100000) exceeded`. Output
  produced before the failure is retained in the writer.
- **Duplicate import**: importing the same path twice returns the same cached
  `*Template` (the loader is consulted once); the merge is idempotent.
- **Loader drift (same path, new contents)**:
  - normal `Set`: the first parse is cached and keeps serving identically;
  - `InDevelopmentMode()`: the cache is bypassed, so new contents are parsed.
- **Cache hit vs cold load**: a warm render and a cold render (same Set or a
  second Set over the same loader) produce identical output and identical
  decisive lookup paths.
- **Two `Set` instances**: caches, globals, escape settings and block tables are
  per-Set; same-named paths in different Sets never pollute each other.
- **Run-time errors**: enabling the observer changes neither allocation on the
  hot path nor error text/location (e.g. errors are attributed to the deepest
  template in an include chain).
- **Auto-escaping**: chosen per Set by `WithSafeWriter` (default
  `text/template.HTMLEscape`; `nil` disables it). `include` shares the Set and
  therefore the same escapee; switching `.` does not change escaping. Built-ins
  `raw`/`unsafe` bypass, `safeHtml`/`safeJs` apply explicit writers.

## Internal observer (test scaffolding)

`observer.go` adds opt-in, **unexported** instrumentation (no public API):

- the observer interface (`lookupObserver`) receives `lookupEvent` (variables/blocks) and
  `parseBindEvent` (block merges).
- A `lookupEvent` records the name, the starting scope depth, every probed
  candidate layer in order, the selected source/depth, the defining template of
  a selected block, and a snapshot of the template call stack
  (`execute` → `include` / `includeIfExists` / `exec`).
- Events are capped (`LookupRecorder.EventCap`, default 4096); retained events
  keep their stack snapshots and truncation is reported, but dropped events add
  no stack-copy cost.
- With no observer attached the lookup paths are the original code paths; the
  hooks are nil-guarded and allocate nothing.

## Complexity

- Variable lookup: O(d) where d is the lexical scope depth (globals/defaults are
  O(1) map lookups).
- Block lookup: O(d) scope-chain steps (O(1) map read per step).
- Parse: O(n) nodes plus the transitive parse of extends/imports; each distinct
  resolved path is parsed at most once per Set (then cached). Block merge is
  O(number of inherited blocks).
- Memory per render: O(d) scopes; an attached observer retains up to the event
  cap, each event carrying O(d) candidate/stack data.

## Compatibility

Nothing here changes public behaviour: the observer is internal, error text,
allocation on the uninstrumented path, supported platforms and template
semantics are unchanged. The fatal nature of parse-time extends/import cycles is
documented as existing behaviour, not altered.

## Rule → function index

| Rule | Function (file) |
|---|---|
| Tokenise `extends/import/include/block/yield/content` | lexer keywords (`lex.go`) |
| Parse extends/import header, load referenced templates | `Template.parseTemplate` (`parse.go`) → `Set.getSiblingTemplate` (`set.go`) |
| Parse block / yield / include nodes | `parseBlock`, `parseYield`, `parseInclude` (`parse.go`) |
| Parse `=` vs `:=` | `Template.assignmentOrExpression` (`parse.go`) |
| Build/merge the block table | `Set.parse`, `Template.bindBlocks` (`parse.go`/`set.go`) |
| Cache vs loader decision, extensions, dev mode | `Set.getTemplate`, `getTemplateFromCache`, `getTemplateFromLoader`, `loadFromFile` (`set.go`) |
| Variable resolution order | `Runtime.resolve`, `Runtime.resolveObserved` (`eval.go`) |
| `=` / `:=` / `LetGlobal` semantics | `Runtime.setValue`, `Set`, `Let`, `SetOrLet`, `executeSetList`, `executeLetList` (`eval.go`) |
| Block table walk | `(*scope).getBlock`, `Runtime.resolveBlock`, `Runtime.lookupBlock` (`eval.go`) |
| Block/yield execution, parameter scopes, content closure | `Runtime.executeYieldBlock` (`eval.go`) |
| Include scope, block-table swap, context switch, depth guard | `Runtime.executeInclude` (`eval.go`) |
| Execute seeding, extends root selection | `Template.Execute`, internal `execute` (`exec.go`) |
| `includeIfExists` / `exec` builtins | built-in `Func`s (`default.go`) |
| Auto-escaping | `Set.escapee`, `escapeeWriter.Write`, `Runtime.evalSafeWriter` (`set.go`/`eval.go`) |
| Scope chain representation | `Runtime`, `scope` (`eval.go`), `VarMap` (`exec.go`) |
