# Name Resolution

This document is the executable map of how Jet binds names. It covers what each
construct binds **at parse time** and what it binds **at execution time**, where
same-named definitions shadow each other, and which function makes the decision.

The rules below are pinned down by characterization tests in
`name_resolution_test.go` (`TestNameResolution_*`). The internal lookup observer
in `observe.go` records the same path the engine actually walks (name, starting
scope, per-layer candidates, winning source and template invocation stack).

## The two phases

| Phase | Owner | Entry points |
|---|---|---|
| Parse | lexer (`lex.go`), parser (`parse.go`, `node.go`, `constructors.go`), `Set` loader (`set.go`, `loader.go`) | `Set.parse`, `Template.parseTemplate` |
| Execute | runtime/scope (`eval.go`, `exec.go`) | `Template.Execute`, `Runtime.executeList` |

Templates are parsed once (then cached per `Set`); variable and block *lookups*
happen on every execution.

## Syntax-by-syntax binding table

| Syntax | Lexer token (`lex.go`) | Bound at parse time | Bound at execution time |
|---|---|---|---|
| `{{extends "p"}}` | `itemExtends` | `Template.parseTemplate` resolves the sibling path through `Set.getSiblingTemplateTraced` and stores `Template.extends`. Must precede every `import`; at most one. | `Template.Execute` walks `t.extends` to the terminal parent and executes **that** root. |
| `{{import "p"}}` | `itemImport` | Same resolution; the parsed template is appended to `Template.imports`. Its blocks are merged into `processedBlocks`. | Imports add **no executable nodes**; imported templates only contribute blocks. |
| `{{include "p" ctx}}` | `itemInclude` | `Template.parseInclude` only builds the `IncludeNode`; the path expression is not resolved. | `Runtime.executeInclude` evaluates the name expression, resolves it relative to the node's `TemplatePath`, creates a fresh scope (`newScope`), installs `t.processedBlocks`, optionally rebinds dot (`node.Context`) and executes the included template's terminal root. Depth is bounded by `enterInclude`/`maximumIncludeDepth`. |
| `{{block n(...) expr}}...{{content}}...{{end}}` | `itemBlock` | `Template.parseBlock` builds a `BlockNode` and registers it in `Template.passedBlocks`; after parsing, `Set.parse` merges blocks (see precedence below). | A `block` node executes the block found under its name (`scope.getBlockProbed`), falling back to itself when absent, through `Runtime.executeYieldBlock`. |
| `{{yield n(...) expr content}}...{{end}}` | `itemYield` | `Template.parseYield` builds a `YieldNode` (name, caller-side parameter list, context expression, inline content). | `Runtime.executeList` calls `scope.getBlockProbed`; the winning block runs with caller parameters overlaid and, for `yield ... content`, the inline content is installed as a closure that runs in the **yielding** scope. |
| `{{yield content}}` | `itemContent` inside yield | Parsed as a content-only `YieldNode` (`IsContent`). | Invokes the current `Runtime.content` closure (if any); does not touch the block map. |
| Identifier / call (`foo`, `foo(a)`) | `itemIdentifier` | `Template.term`/`operand` produce `IdentifierNode`/`CallExprNode`; no value exists yet. | `Runtime.evalBaseExpressionGroup` (`NodeIdentifier`) calls `Runtime.resolve`; calls then go through `Runtime.evalCallExpression`. |
| Field / chain (`.x`, `a.b`) | `itemField` | `FieldNode`/`ChainNode` are built; dot and field names are just strings. | Fields never consult variable scopes: the chain starts at `Runtime.context` (`.`) or a resolved identifier and each segment goes through `resolveIndex`. |
| `x = v` (assignment) | `itemAssign` (`=`) | `Template.assignmentOrExpression` builds `SetNode{Let:false}`. | `Runtime.executeSetList` → `Runtime.setValue`: walks current→parent scopes and mutates the first existing binding; error if none exists. Globals/defaults are **not** assignment targets. |
| `x := v` (declaration) | `itemAssign` (`:=`) | `SetNode{Let:true}`; left side must be identifiers/`_`. | `Runtime.executeLetList` writes into the **current** scope (a new one is created for the enclosing list/if/range), shadowing any outer name. |

## Variable precedence (Runtime.resolve)

`Runtime.resolve` checks layers in this exact order; the first layer containing
the name wins:

1. Each execution scope, inner to outer — `Runtime.newScope`/`releaseScope`.
   The root scope (`Runtime.rootScope`) is the **template** layer holding the
   `VarMap` passed to `Template.Execute`; scopes created during execution are
   **local** (if/range/try with `:=`, block/yield parameters, includes).
2. `Set` globals — `Set.AddGlobal`/`AddGlobalFunc`, guarded by `Set.gmx`.
3. Built-in defaults — `defaultVariables` (`default.go`: `lower`, `len`,
   `html`, `raw`, `map`, `includeIfExists`, `exec`, ...).
4. `.` is not a variable: it returns `Runtime.context` directly.
5. Miss produces the stable error
   `identifier %q not available in current (...) or parent scope, global, or
   default variables`.

Assignment (`=`) and declaration (`:=`) use the same scope chain but different
write rules (`setValue` vs `executeLetList`); `Runtime.LetGlobal` targets the
top-most execution scope.

## Block precedence (decided at parse time)

Blocks ("macros") are plain `BlockNode`s merged into one map in `Set.parse`:

1. `t.addBlocks(t.extends.processedBlocks)` — extended parent first;
2. `for _, _import := range t.imports { t.addBlocks(...) }` — imports in
   textual order, so a **later** import overrides an earlier one;
3. `t.addBlocks(t.passedBlocks)` — blocks declared in the current template win
   over everything.

Consequences:

- The decisive selection is a deterministic, ordered merge — never Go map
  iteration order. At execution, `scope.getBlockProbed` does one map lookup in
  `scope.blocks` (the parent walk only matters because includes swap the whole
  map; ordinary execution scopes share the same block map).
- `extends` itself contributes no output surface beyond the parent's root; a
  child's block renders only where the parent `yield`s its name.
- An included template runs against **its own** `processedBlocks`, not the
  caller's, and a fresh variable scope: callers only pass dot (context), never
  template variables.
- `yield name(...)` parameters bind in a new scope inside
  `executeYieldBlock`; caller-provided values override block defaults, and a
  `content` closure executes with the yielding scope restored.

## Template path resolution (Set/loader)

`Set.getTemplateTraced` (factored from the cache/loader pipeline) performs, in
order:

1. cache probes for each entry of `Set.extensions`
   (`getTemplateFromCache`, default `""`, `.jet`, `.html.jet`, `.jet.html`);
2. otherwise loader probes `Loader.Exists` for each extension
   (`getTemplateFromLoader`), then `loadFromFile` → `Set.parse`;
3. a successful loader parse is stored via `Cache.Put` (skipped in development
   mode and for `Set.Parse`/uncached import resolution).

Sibling resolution (`getSiblingTemplateTraced`) joins the referencing
template's directory with relative paths first (`path`/`filepath`), which is
why the same template is always reached by the same canonical path.

Guarantees and sharp edges:

- A cache hit returns the exact cached `*Template`, so warm and cold
  resolutions render identically as long as the loader contents are unchanged;
  mutating the loader after first parse does not affect cached templates (use
  `InDevelopmentMode` to bypass the cache).
- Caches and globals are per-`Set`; two sets never share state unless the same
  `Cache` implementation is handed to both via `WithCache`.
- If two sets **do** share a cache but use different `SafeWriter`s, a cached
  template keeps the pointer to the `Set` that parsed it (`Template.set`): the
  second set's cache hit escapes through the first set's escape policy. Prefer
  separate caches per escape configuration. Per-command writers (`raw`,
  `safeHtml`, `safeJs`, registered in `defaultVariables`) only switch escaping
  for that command.

## Circular references and errors

- A cyclic `extends`/`import` chain recurses through the parser; nothing is
  cached until a parse finishes, so the cycle ends in an unrecoverable stack
  overflow (characterized in a subprocess with a bounded stack).
- Cyclic `include`s execute at runtime and are bounded by
  `Runtime.maximumIncludeDepth`; deep cycles exhaust Go stack frames near the
  bound and fail the run (never hang).
- Parse errors use `template: <name>:<line>: ...` (from
  `Template.errorf`); runtime errors use
  `Jet Runtime Error ("<path>":<line>): ...` (from `NodeBase.errorf`).

## Lookup observer (internal scaffold)

`observe.go` adds an opt-in probe used only by tests:

- `attachLookupObserver(set, observer, limit)` / `detachLookupObserver` are
  unexported; the public API is unchanged.
- Instrumented points: `Runtime.resolve` (reads), `Runtime.setValue`
  (assignments), `executeLetList`/`Let`/`LetGlobal` (declarations),
  `scope.getBlockProbed` (blocks), `Set.getTemplateTraced` (cache/loader
  probes) and template frames pushed in `Template.Execute`/`executeInclude`.
- When no observer is attached each site is a single nil check; the original
  lookup loop still exists byte-for-byte for the uninstrumented path, so
  scheduling, allocations for rendering itself and error text are unchanged.
- Events are capped (`DefaultObserverEventLimit` when unset); once the cap is
  reached further events are dropped while every retained event still carries
  the full template invocation stack.
- Events expose ordered slices only (extension probes, scope layers). They do
  not report map iteration order, which the engine never relies on.

## Complexity

- Variable resolution is O(d) in the number of open execution scopes plus two
  map lookups (globals, defaults); assignment is O(d) on the same chain.
- Block resolution after parse is O(1) map access; block merging is O(b) per
  parse.
- Template resolution is O(e) cache probes and, on a miss, O(e) `Exists`
  probes plus one parse, where e is `len(Set.extensions)` (default 4).
- The observer adds O(d) candidate recording per instrumented lookup and
  O(1) amortized per bounded event append; it is inert when disabled.

## Compatibility notes

- No public types, functions, error strings or default behavior changed; the
  observer is internal and attached per `Set`.
- The shared-cache/different-`SafeWriter` behavior and cyclic-parse overflow
  are pre-existing characteristics, now documented and tested rather than
  altered.
- Tests use the in-memory loader only: no network, real-clock waits or
  filesystem-traversal order assumptions; overflow scenarios run in child
  processes with bounded stacks and `GOGC=off`.
