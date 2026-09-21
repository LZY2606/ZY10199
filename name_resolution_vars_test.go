package jet

import (
	"strings"
	"testing"
)

// Variable resolution order (see docs/name-resolution.md):
//
//	"." -> context
//	identifier -> execute scope (depth 0) -> inner scopes (block/range/if/try
//	             parameters, deepest-first on the chain) -> Set globals ->
//	             built-in defaults -> unresolved error
//
// Assignment: "=" walks the chain and mutates the first existing binding; ":="
// creates a binding in the current scope (shadowing).

func TestNameResolution_Variable_LocalShadowsGlobalShadowsDefault(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/page.jet", `{{upper("abc")}}`)
	// With no execute/global "upper", the built-in default function wins.
	res := renderObserved(t, loader, "/page.jet", nil, nil)
	ev := res.mustRender(t).lookup(t, lookupVariable, "upper")
	expectSelected(t, ev, sourceDefault)
	res.expectOut(t, "ABC")
}

func TestNameResolution_Variable_ExecuteScopeSeedsVars(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/page.jet", `v={{v}}`)
	res := renderObserved(t, loader, "/page.jet", VarMap{"v": rv("seed")}, nil)
	ev := res.mustRender(t).expectOut(t, "v=seed").lookup(t, lookupVariable, "v")
	expectSelected(t, ev, sourceExecuteScope)
	if ev.StartAt != 0 || ev.SelectedDepth != 0 {
		t.Fatalf("expected execute-scope lookup at depth 0: %s", ev.Describe())
	}
}

func TestNameResolution_Variable_LetShadowsExecuteScope(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/page.jet", `{{ v := "inner" }}[{{v}}]`)
	res := renderObserved(t, loader, "/page.jet", VarMap{"v": rv("outer")}, nil)
	// The read of v happens after the := inside one extra scope, so it resolves
	// at depth 1 (local), shadowing depth 0 (execute vars).
	ev := res.mustRender(t).expectOut(t, "[inner]").lookup(t, lookupVariable, "v")
	expectSelected(t, ev, sourceLocalScope)
	if ev.SelectedDepth != 1 {
		t.Fatalf("expected shadowing local binding at depth 1: %s", ev.Describe())
	}
}

func TestNameResolution_Variable_SetMutatesExistingBinding(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/page.jet", `[{{v}}]{{ v = "changed" }}[{{v}}]`)
	res := renderObserved(t, loader, "/page.jet", VarMap{"v": rv("original")}, nil)
	res.mustRender(t).expectOut(t, "[original][changed]")
	// Every read/walk sees the binding in the execute scope: no new scope, no
	// local shadow.
	for _, ev := range res.rec.Events() {
		if ev.Kind == lookupVariable && ev.Name == "v" {
			expectSelected(t, ev, sourceExecuteScope)
		}
	}
}

func TestNameResolution_Variable_SetUninitialisedErrors(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/page.jet", `{{ v = "changed" }}`)
	res := renderObserved(t, loader, "/page.jet", nil, nil)
	res.expectErr(t, `could not assign "v" = changed because variable "v" is uninitialised`)
}

func TestNameResolution_Variable_RangeLetIsLocalScope(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/page.jet", `{{range item := .}}<{{item}}>{{end}}`)
	// A single let-variable binds the ranger's index (see scopeDepth semantics).
	res := renderObserved(t, loader, "/page.jet", nil, []string{"a", "b"})
	res.mustRender(t).expectOut(t, "<0><1>")
	ev := res.lookup(t, lookupVariable, "item")
	if ev.SelectedDepth < 1 {
		t.Fatalf("range variable must resolve from a scope above execute scope: %s", ev.Describe())
	}
}

func TestNameResolution_Variable_BlockParametersAreLocal(t *testing.T) {
	loader := NewInMemLoader()
	// Required-param blocks are invoked immediately at definition time, so host
	// them via import; the yield supplies named parameters into a new scope.
	loader.Set("/lib.jet", `{{block greet(who)}}hi {{who}}{{end}}`)
	loader.Set("/page.jet", `{{import "/lib.jet"}}{{yield greet(who="world")}}`)
	res := renderObserved(t, loader, "/page.jet", nil, nil)
	ev := res.mustRender(t).expectOut(t, "hi world").lookup(t, lookupVariable, "who")
	if ev.SelectedDepth < 1 {
		t.Fatalf("block parameter must live in a local scope: %s", ev.Describe())
	}
}

func TestNameResolution_Variable_ContextDot(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/page.jet", `dot={{.}}`)
	res := renderObserved(t, loader, "/page.jet", nil, "CTX")
	ev := res.mustRender(t).expectOut(t, "dot=CTX").lookup(t, lookupVariable, ".")
	expectSelected(t, ev, sourceContext)
}

func TestNameResolution_Variable_UnresolvedErrorAndCandidates(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/page.jet", `{{ghost}}`)
	res := renderObserved(t, loader, "/page.jet", nil, nil)
	res.expectErr(t, `identifier "ghost" not available`)
	ev := res.lookup(t, lookupVariable, "ghost")
	if ev.Found {
		t.Fatalf("ghost should not resolve: %s", ev.Describe())
	}
	expectSelected(t, ev, sourceUnresolved)
	// The walk must probe execute scope, globals and defaults in that order, all
	// misses, which is why the terminal error mentions all three layers.
	expectCandidates(t, ev,
		lookupCandidate{Layer: "scope:0", Present: false},
		lookupCandidate{Layer: string(sourceGlobal), Present: false},
		lookupCandidate{Layer: string(sourceDefault), Present: false},
	)
}

func TestNameResolution_Variable_GlobalBeatsDefault(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/page.jet", `{{upper}}`)
	set := NewSet(loader)
	set.AddGlobal("upper", "GLOBAL")
	tpl, err := set.GetTemplate("/page.jet")
	if err != nil {
		t.Fatal(err)
	}
	rec := NewLookupRecorder()
	set.parseObserver = rec
	var buf strings.Builder
	if err := tpl.execute(&buf, nil, nil, rec); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "GLOBAL" {
		t.Fatalf("global must shadow default, got %q", buf.String())
	}
	ev := rec.findEvent(lookupVariable, "upper")
	if ev == nil {
		t.Fatal("upper lookup not recorded")
	}
	expectSelected(t, *ev, sourceGlobal)
}
