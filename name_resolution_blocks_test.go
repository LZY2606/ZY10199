package jet

import (
	"testing"
)

// Block ("macro") resolution:
//
// Parse time: every template ends up with one processedBlocks table. The merge
// order in (*Set).parse is:
//
//	extends table first  (lowest precedence)
//	import tables next  (later import statements win on name collisions)
//	self blocks last    (highest precedence; child overrides parent)
//
// Run time: {{yield name}} looks the block tables up through the lexical scope
// chain via scope.getBlock, then executes the winning block's list. {{block}}
// definitions are also invoked immediately. Imported templates are never
// executed, so their variables never exist, only their block lists.

func TestNameResolution_Block_DuplicateImport_LaterWins(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/lib1.jet", `{{block widget()}}L1{{end}}`)
	loader.Set("/lib2.jet", `{{block widget()}}L2{{end}}`)
	loader.Set("/page.jet", `{{import "/lib1.jet"}}{{import "/lib2.jet"}}P{{yield widget()}}`)

	res := renderObserved(t, loader, "/page.jet", nil, nil)
	res.mustRender(t).expectOut(t, "PL2")

	ev := res.lookup(t, lookupBlock, "widget")
	expectSelected(t, ev, sourceBlockTable)
	if ev.SelectedTemplate != "/lib2.jet" {
		t.Fatalf("later import must win block collision, selected origin=%s\n%s", ev.SelectedTemplate, ev.Describe())
	}

	// Parse-time evidence: two bindings into /page.jet for widget, the second
	// (lib2) recorded as shadowing the first.
	bindings := res.parseBindingsFor(t, "widget")
	var pageBindings []parseBindEvent
	for _, b := range bindings {
		if b.Template == "/page.jet" {
			pageBindings = append(pageBindings, b)
		}
	}
	if len(pageBindings) != 2 {
		t.Fatalf("expected two widget bindings into page, got %+v", pageBindings)
	}
	if !pageBindings[1].Shadowed {
		t.Fatalf("the second import binding must report a shadow: %+v", pageBindings)
	}
}

func TestNameResolution_Block_DuplicateImport_OrderReversed(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/lib1.jet", `{{block widget()}}L1{{end}}`)
	loader.Set("/lib2.jet", `{{block widget()}}L2{{end}}`)
	loader.Set("/page.jet", `{{import "/lib2.jet"}}{{import "/lib1.jet"}}P{{yield widget()}}`)

	res := renderObserved(t, loader, "/page.jet", nil, nil)
	res.mustRender(t).expectOut(t, "PL1")
	ev := res.lookup(t, lookupBlock, "widget")
	if ev.SelectedTemplate != "/lib1.jet" {
		t.Fatalf("with reversed import order lib1 must win, got %s\n%s", ev.SelectedTemplate, ev.Describe())
	}
}

func TestNameResolution_Block_LocalOverridesImport(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/lib1.jet", `{{block widget()}}L1{{end}}`)
	loader.Set("/lib2.jet", `{{block widget()}}L2{{end}}`)
	loader.Set("/page.jet", `{{import "/lib1.jet"}}{{import "/lib2.jet"}}{{block widget()}}LOCAL{{end}}{{yield widget()}}`)

	res := renderObserved(t, loader, "/page.jet", nil, nil)
	// The inline definition runs immediately ("LOCAL") and the later yield also
	// resolves to the local block.
	res.mustRender(t).expectOut(t, "LOCALLOCAL")
	var yieldLookup []lookupEvent
	for _, e := range res.rec.Events() {
		if e.Kind == lookupBlock && e.Name == "widget" {
			yieldLookup = append(yieldLookup, e)
		}
	}
	if len(yieldLookup) == 0 {
		t.Fatal("no widget block lookups recorded")
	}
	last := yieldLookup[len(yieldLookup)-1]
	if last.SelectedTemplate != "/page.jet" {
		t.Fatalf("self block must override imports, got origin=%s\n%s", last.SelectedTemplate, last.Describe())
	}
	// And the final ("self") parse binding must shadow the import winner.
	bindings := res.parseBindingsFor(t, "widget")
	lastBinding := bindings[len(bindings)-1]
	if lastBinding.Phase != "self" || !lastBinding.Shadowed {
		t.Fatalf("self binding should shadow imports: %+v", lastBinding)
	}
}

func TestNameResolution_Block_ExtendsChildOverridesParent(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/layout.jet", `L[{{yield body()}}]`)
	loader.Set("/child.jet", `{{extends "/layout.jet"}}{{block body()}}CHILD{{end}}discarded`)

	res := renderObserved(t, loader, "/child.jet", nil, nil)
	res.mustRender(t).expectOut(t, "L[CHILD]")

	ev := res.lookup(t, lookupBlock, "body")
	if ev.SelectedTemplate != "/child.jet" {
		t.Fatalf("child block must override parent default, got %s", ev.SelectedTemplate)
	}
	// Parse-time mechanism: when /layout.jet was parsed (lazily, during the
	// child's parse), /child.jet was its pending child and already contributed
	// body into the *same* processedBlocks table via the "self" merge. The
	// later "extends" merge therefore copies the already-overridden block and
	// does not bind a parent-origin body. This is why the child wins.
	bindings := res.parseBindingsFor(t, "body")
	var selfOrigin string
	for _, b := range bindings {
		if b.Template == "/child.jet" && b.Phase == "self" {
			selfOrigin = b.Origin
		}
	}
	if selfOrigin != "/child.jet" {
		t.Fatalf("expected child-origin self body binding, got %+v", bindings)
	}
}

func TestNameResolution_Block_ParentDefaultSurvivesWhenNotOverridden(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/layout.jet", `{{block footer()}}DEFAULT-FOOTER{{end}}`)
	loader.Set("/child.jet", `{{extends "/layout.jet"}}{{block body()}}BODY{{end}}`)

	res := renderObserved(t, loader, "/child.jet", nil, nil)
	res.mustRender(t).expectOut(t, "DEFAULT-FOOTER")
	ev := res.lookup(t, lookupBlock, "footer")
	if ev.SelectedTemplate != "/layout.jet" {
		t.Fatalf("unoverridden block must come from parent, got %s", ev.SelectedTemplate)
	}
}

func TestNameResolution_Block_YieldContentCapturesScope(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/lib.jet", `{{block card(title)}}<div title="{{title}}">{{yield content}}</div>{{end}}`)
	loader.Set("/page.jet", `{{import "/lib.jet"}}{{yield card(title="T") content}}body-{{x}}{{end}}`)

	res := renderObserved(t, loader, "/page.jet", VarMap{"x": rv("XV")}, nil)
	res.mustRender(t).expectOut(t, `<div title="T">body-XV</div>`)

	// The content closure restores the yield site's scope, so x resolves against
	// the page's execute scope, not the block's parameter scope.
	xev := res.lookup(t, lookupVariable, "x")
	expectSelected(t, xev, sourceExecuteScope)
	// title resolves inside the block list from the parameter (local) scope.
	tev := res.lookup(t, lookupVariable, "title")
	if tev.SelectedDepth < 1 {
		t.Fatalf("block parameter must be read from a local scope: %s", tev.Describe())
	}
}

func TestNameResolution_Block_YieldContextSwitchesDot(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/lib.jet", `{{block row()}}<{{.}}>{{end}}`)
	loader.Set("/page.jet", `{{import "/lib.jet"}}{{yield row() "CTX"}}`)

	res := renderObserved(t, loader, "/page.jet", nil, nil)
	res.mustRender(t).expectOut(t, "<CTX>")
	dot := res.lookup(t, lookupVariable, ".")
	if res.out != "<CTX>" {
		t.Fatalf("bad output %q", res.out)
	}
	_ = dot
}

func TestNameResolution_Block_UnresolvedYieldErrors(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/page.jet", `{{yield ghost()}}`)
	res := renderObserved(t, loader, "/page.jet", nil, nil)
	res.expectErr(t, `unresolved block "ghost"`)
	ev := res.lookup(t, lookupBlock, "ghost")
	if ev.Found {
		t.Fatalf("ghost block should be absent: %s", ev.Describe())
	}
	for _, c := range ev.Candidates {
		if c.Present {
			t.Fatalf("no block table should contain ghost: %s", ev.Describe())
		}
	}
}

func TestNameResolution_Block_ImportedVariablesDoNotExist(t *testing.T) {
	loader := NewInMemLoader()
	// A let-bound variable inside an imported block only exists when the block
	// runs; here it renders fine inside its own scope.
	loader.Set("/lib.jet", `{{block show()}}{{ v := "LIB" }}{{v}}{{end}}`)
	loader.Set("/page.jet", `{{import "/lib.jet"}}{{yield show()}};isset={{isset(v)}}`)
	res := renderObserved(t, loader, "/page.jet", nil, nil)
	res.mustRender(t).expectOut(t, "LIB;isset=false")
}

func TestNameResolution_Block_ParseBindingsPhaseOrderAndOrigins(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/base.jet", `{{block keep()}}BASE{{end}}`)
	loader.Set("/lib1.jet", `{{block only1()}}L1{{end}}{{block keep()}}L1KEEP{{end}}`)
	loader.Set("/lib2.jet", `{{block only2()}}L2{{end}}`)
	loader.Set("/page.jet", `{{extends "/base.jet"}}{{import "/lib1.jet"}}{{import "/lib2.jet"}}{{block keep()}}PAGE{{end}}`)

	res := renderObserved(t, loader, "/page.jet", nil, nil)
	res.mustRender(t)

	pageBindings := res.parseBindingsOn("/page.jet")
	if len(pageBindings) == 0 {
		t.Fatal("expected parse bindings into /page.jet")
	}

	// Phase order across the page table must be extends < import < self. Order
	// among distinct *names* inside one phase is not asserted (map iteration).
	rank := map[string]int{"extends": 0, "import": 1, "self": 2}
	last := -1
	for _, b := range pageBindings {
		if rank[b.Phase] < last {
			t.Fatalf("binding phase %s arrived out of merge order: %+v", b.Phase, pageBindings)
		}
		last = rank[b.Phase]
	}

	// The distinct contributing origins, asserted order-independently.
	origins := sortedOrigins(pageBindings)
	wantOrigins := []string{"/base.jet", "/lib1.jet", "/lib2.jet", "/page.jet"}
	if len(origins) != len(wantOrigins) {
		t.Fatalf("expected origins %v, got %v", wantOrigins, origins)
	}
	for i := range wantOrigins {
		if origins[i] != wantOrigins[i] {
			t.Fatalf("expected sorted origins %v, got %v", wantOrigins, origins)
		}
	}

	// "keep" is bound four times across phases; the final self binding shadows.
	keep := res.parseBindingsFor(t, "keep")
	final := keep[len(keep)-1]
	if final.Origin != "/page.jet" || final.Phase != "self" || !final.Shadowed {
		t.Fatalf("expected final self shadow for keep, got %+v", final)
	}
}

func TestNameResolution_Block_CandidateLayersRecordedInOrder(t *testing.T) {
	// A yield inside an include probes the included table (found there), while
	// a miss probes each block-table scope before failing.
	loader := NewInMemLoader()
	loader.Set("/lib.jet", `{{block present()}}HERE{{end}}`)
	loader.Set("/part.jet", `{{import "/lib.jet"}}{{yield present()}}`)
	loader.Set("/page.jet", `{{include "/part.jet"}}`)
	res := renderObserved(t, loader, "/page.jet", nil, nil)
	res.mustRender(t).expectOut(t, "HERE")
	ev := res.lookup(t, lookupBlock, "present")
	if len(ev.Candidates) != 1 || !ev.Candidates[0].Present {
		t.Fatalf("block should be found in the first probed table: %s", ev.Describe())
	}
}
