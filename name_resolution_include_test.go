package jet

import (
	"strings"
	"testing"
)

// include is a run-time-only construct (nothing is bound at parse time except
// the IncludeNode). executeInclude resolves the name expression, loads via the
// Set, opens a new lexical scope (parented to the current one, so all variables
// remain visible), swaps in the included template's block table, optionally
// switches "." to the passed context, and runs the (extends-resolved) root.

func TestNameResolution_Include_SeesOuterVarsAndGlobals(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/part.jet", `name={{.}};g={{g}};d={{upper("x")}}`)
	loader.Set("/page.jet", `{{include "/part.jet" .Name}}`)

	res := renderObserved(t, loader, "/page.jet", VarMap{"g": rv("GV")}, struct{ Name string }{"n1"})
	res.mustRender(t).expectOut(t, "name=n1;g=GV;d=X")

	ev := res.lookup(t, lookupVariable, "g")
	expectSelected(t, ev, sourceExecuteScope)
	expectStack(t, ev, "execute:/page.jet", "include:/part.jet")
}

func TestNameResolution_Include_ContextSwitchIsScoped(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/part.jet", `<{{.}}>`)
	loader.Set("/page.jet", `({{.}}){{include "/part.jet" "INNER"}}[{{.}}]`)

	res := renderObserved(t, loader, "/page.jet", nil, "OUTER")
	res.mustRender(t).expectOut(t, "(OUTER)<INNER>[OUTER]")

	var inside, outside int
	for _, ev := range res.rec.Events() {
		if ev.Kind == lookupVariable && ev.Name == "." {
			if len(ev.Stack) == 2 && ev.Stack[1].Template == "/part.jet" {
				inside++
			} else {
				outside++
			}
		}
	}
	if inside != 1 || outside != 2 {
		t.Fatalf("expected 1 inner and 2 outer context reads, got %d/%d", inside, outside)
	}
}

func TestNameResolution_Include_UsesIncludedBlockTable(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/part.jet", `{{yield badge()}}`)
	loader.Set("/badge_lib.jet", `{{block badge()}}PART-BADGE{{end}}`)
	loader.Set("/page.jet", `{{include "/part.jet"}}`)
	// part imports its own badge library
	loader.Set("/part.jet", `{{import "/badge_lib.jet"}}{{yield badge()}}`)

	res := renderObserved(t, loader, "/page.jet", nil, nil)
	res.mustRender(t).expectOut(t, "PART-BADGE")
	ev := res.lookup(t, lookupBlock, "badge")
	if ev.SelectedTemplate != "/badge_lib.jet" {
		t.Fatalf("include must run with included block table, got %s", ev.SelectedTemplate)
	}
	expectStack(t, ev, "execute:/page.jet", "include:/part.jet")
}

func TestNameResolution_Include_NameResolvedAtRunTime(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/p1.jet", `ONE`)
	loader.Set("/p2.jet", `TWO`)
	loader.Set("/page.jet", `{{include which}}`)

	res := renderObserved(t, loader, "/page.jet", VarMap{"which": rv("/p1.jet")}, nil)
	res.mustRender(t).expectOut(t, "ONE")
	// Changing the variable selects a different template with no parse change.
	res2 := renderObserved(t, loader, "/page.jet", VarMap{"which": rv("/p2.jet")}, nil)
	res2.mustRender(t).expectOut(t, "TWO")
}

func TestNameResolution_Include_MissingTemplateErrors(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/page.jet", `a{{include "/nope.jet"}}b`)
	res := renderObserved(t, loader, "/page.jet", nil, nil)
	res.expectErr(t, `template /nope.jet could not be found`)
}

// Combined scenario exercising the whole graph in one render:
// extends (block override) + two same-named imports + include with a context +
// yield content + local := shadowing + globals/defaults + block parameters.
func TestNameResolution_CombinedGraph(t *testing.T) {
	loader := NewInMemLoader()

	// Parent layout yields an overridable block (supplying its parameter), an
	// imported macro, a slot block, includes a partial with a switched context,
	// and reads globals/defaults.
	loader.Set("/layout.jet", `LAYOUT{`+
		`main={{yield main(title="T0")}};`+
		`macro={{yield widget()}};`+
		`partial={{include "/partial.jet" .}};`+
		`g={{g}};up={{upper("a")}};`+
		`slot={{yield slot()}}`+
		`}`)
	loader.Set("/partial.jet", `P<{{.ID}}:{{local}}>`)

	// Two libraries defining the same macro name.
	loader.Set("/lib_a.jet", `{{block widget()}}A-WIDGET{{end}}`)
	loader.Set("/lib_b.jet", `{{block widget()}}B-WIDGET{{end}}`)
	// A parameterised content-macro library.
	loader.Set("/lib_card.jet", `{{block card(text)}}CARD[{{text}}|{{yield content}}]{{end}}`)

	// Child: extends layout, imports both widget libraries (B later -> wins) and
	// the card library, overrides main (reading its yield-supplied parameter)
	// and fills the slot with a yield-with-content invocation.
	loader.Set("/child.jet", `{{extends "/layout.jet"}}`+
		`{{import "/lib_a.jet"}}{{import "/lib_b.jet"}}{{import "/lib_card.jet"}}`+
		`{{block main(title)}}M<{{title}}:{{local}}>{{end}}`+
		`{{block slot()}}{{yield card(text="CT") content}}BODY-{{local}}{{end}}{{end}}`)

	set := NewSet(loader)
	set.AddGlobal("g", "GG")
	tpl, err := set.GetTemplate("/child.jet")
	if err != nil {
		t.Fatal(err)
	}
	rec := NewLookupRecorder()
	set.parseObserver = rec
	var localHolder struct {
		ID int
	}
	localHolder.ID = 7
	// local is seeded in the execute scope and stays visible across the layout
	// yields, the include and the captured content closure.
	var buf strings.Builder
	err = tpl.execute(&buf, VarMap{"local": rv("LOC")}, localHolder, rec)
	if err != nil {
		t.Fatalf("combined render: %v", err)
	}

	want := `LAYOUT{main=M<T0:LOC>;macro=B-WIDGET;partial=P<7:LOC>;g=GG;up=A;slot=CARD[CT|BODY-LOC]}`
	if buf.String() != want {
		t.Fatalf("combined graph mismatch:\n\twant %s\n\tgot  %s", want, buf.String())
	}

	// Decisive-path assertions.
	mainEv := rec.findEvent(lookupBlock, "main")
	if mainEv == nil || mainEv.SelectedTemplate != "/child.jet" {
		t.Fatalf("main must resolve to child override: %+v", mainEv)
	}
	widgetEv := rec.findEvent(lookupBlock, "widget")
	if widgetEv == nil || widgetEv.SelectedTemplate != "/lib_b.jet" {
		t.Fatalf("widget must resolve to later import lib_b: %+v", widgetEv)
	}

	// "local" is read from three template sites (main block, partial include,
	// captured content) but always binds to the execute scope (depth 0).
	localReads := 0
	for _, ev := range rec.Events() {
		if ev.Kind == lookupVariable && ev.Name == "local" {
			localReads++
			expectSelected(t, ev, sourceExecuteScope)
		}
	}
	if localReads < 2 {
		t.Fatalf("expected several local reads all at execute scope, got %d", localReads)
	}

	// The partial's ID field read happens with an include frame on the stack.
	gEv := rec.findEvent(lookupVariable, "g")
	if gEv == nil {
		t.Fatal("global g lookup missing")
	}
	expectSelected(t, *gEv, sourceGlobal)

	// The yield-supplied block parameter title must be a local-scope binding.
	titleEv := rec.findEvent(lookupVariable, "title")
	if titleEv == nil || titleEv.SelectedDepth < 1 {
		t.Fatalf("block parameter title should be local scope: %+v", titleEv)
	}
	// The card macro's text parameter likewise binds in a local scope.
	textEv := rec.findEvent(lookupVariable, "text")
	if textEv == nil || textEv.SelectedDepth < 1 {
		t.Fatalf("block parameter text should be local scope: %+v", textEv)
	}
}
