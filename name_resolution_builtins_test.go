package jet

import (
	"bytes"
	"testing"
)

// The includeIfExists and exec builtins run another template's root like
// include does; their lookups must carry their own template-stack frame kind.
func TestNameResolution_BuiltinTemplateFrames(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/page.jet", `{{if includeIfExists("/other.jet", .)}}yes{{end}}|{{exec("/ret.jet")}}|done`)
	loader.Set("/other.jet", `OTHER-{{x}}`)
	loader.Set("/ret.jet", `RET-{{x}}`)

	set := NewSet(loader)
	rec := NewLookupRecorder()
	set.parseObserver = rec
	tpl, err := set.GetTemplate("/page.jet")
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := tpl.execute(&b, VarMap{"x": rv("X")}, nil, rec); err != nil {
		t.Fatal(err)
	}
	// includeIfExists renders inline; exec discards its output (hidden), then
	// the return value (nil here) prints nothing.
	if b.String() != "OTHER-Xyes||done" {
		t.Fatalf("unexpected output %q", b.String())
	}

	sawIncludeIfExists, sawExec := false, false
	for _, ev := range rec.Events() {
		if ev.Kind == lookupVariable && ev.Name == "x" {
			top := ev.Stack[len(ev.Stack)-1].String()
			switch top {
			case "includeIfExists:/other.jet":
				sawIncludeIfExists = true
			case "exec:/ret.jet":
				sawExec = true
			}
		}
	}
	if !sawIncludeIfExists || !sawExec {
		t.Fatalf("expected distinct template frames, got includeIfExists=%v exec=%v", sawIncludeIfExists, sawExec)
	}

	rec.Reset()
	if len(rec.Events()) != 0 || len(rec.ParseEvents()) != 0 || len(rec.LastStack()) != 0 || rec.Truncated() {
		t.Fatal("Reset() must clear events, parse bindings, stack and truncation")
	}
}

func TestNameResolution_BuiltinIncludeIfExistsHidesMissing(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/page.jet", `[{{if includeIfExists("/missing.jet")}}hit{{else}}miss{{end}}]`)
	res := renderObserved(t, loader, "/page.jet", nil, nil)
	res.mustRender(t).expectOut(t, "[miss]")
}
