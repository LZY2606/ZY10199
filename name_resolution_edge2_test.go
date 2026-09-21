package jet

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
)

// countingLoader wraps InMemLoader and counts Exists/Open calls per path so
// tests can assert cache-hit vs cold-load behaviour.
type countingLoader struct {
	inner   *InMemLoader
	mu      sync.Mutex
	existsN map[string]int
	openN   map[string]int
}

func (l *countingLoader) Exists(path string) bool {
	l.mu.Lock()
	if l.existsN == nil {
		l.existsN = map[string]int{}
	}
	l.existsN[path]++
	l.mu.Unlock()
	return l.inner.Exists(path)
}

func (l *countingLoader) Open(path string) (io.ReadCloser, error) {
	l.mu.Lock()
	if l.openN == nil {
		l.openN = map[string]int{}
	}
	l.openN[path]++
	l.mu.Unlock()
	return l.inner.Open(path)
}

func (l *countingLoader) existsCalls(path string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.existsN[path]
}

// --- two Sets must not pollute each other -----------------------------------

func TestNameResolution_TwoSetsDoNotPollute(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/shared", `{{yield marker()}}`)
	loader.Set("/a_marker", `{{block marker()}}A{{end}}`)
	loader.Set("/b_marker", `{{block marker()}}B{{end}}`)
	loader.Set("/pageA", `{{import "/a_marker"}}{{include "/shared"}}`)
	loader.Set("/pageB", `{{import "/b_marker"}}{{include "/shared"}}`)

	render := func(entry string) string {
		rec := NewLookupRecorder()
		set := NewSet(loader, func(s *Set) { s.parseObserver = rec })
		tpl, err := set.GetTemplate(entry)
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err := tpl.execute(&b, nil, nil, rec); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}

	if got := render("/pageA"); got != "A" {
		t.Fatalf("pageA expected A, got %q", got)
	}
	if got := render("/pageB"); got != "B" {
		t.Fatalf("pageB expected B, got %q", got)
	}
	// Interleave again to rule out ordering/cross-contamination.
	if got := render("/pageA"); got != "A" {
		t.Fatalf("second pageA expected A, got %q", got)
	}
	if got := render("/pageB"); got != "B" {
		t.Fatalf("second pageB expected B, got %q", got)
	}
}

// --- autoescape context switching ------------------------------------------

func TestNameResolution_AutoescapeDefault(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/p", `{{x}}|{{raw(x)}}|{{safeHtml(x)}}`)
	rec := NewLookupRecorder()
	set := NewSet(loader, func(s *Set) { s.parseObserver = rec })
	tpl, _ := set.GetTemplate("/p")
	var b bytes.Buffer
	if err := tpl.execute(&b, VarMap{"x": rv("<b>")}, nil, rec); err != nil {
		t.Fatal(err)
	}
	// The default SafeWriter is text/template.HTMLEscape, so a normal action is
	// escaped. raw/unsafe bypasses escaping; safeHtml explicitly re-applies HTML
	// escaping (same bytes as the default path here).
	want := `&lt;b&gt;|<b>|&lt;b&gt;`
	if b.String() != want {
		t.Fatalf("autoescape mismatch:\n\twant %q\n\tgot  %q", want, b.String())
	}
}

func TestNameResolution_AutoescapeDisabledAndSwitched(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/p", `{{x}}|{{raw(x)}}`)

	// Same loader, one Set with escaping off.
	setRaw := NewSet(loader, WithSafeWriter(nil))
	tpl, _ := setRaw.GetTemplate("/p")
	var b bytes.Buffer
	if err := tpl.Execute(&b, VarMap{"x": rv("<b>")}, nil); err != nil {
		t.Fatal(err)
	}
	if b.String() != `<b>|<b>` {
		t.Fatalf("escaping should be disabled per-Set, got %q", b.String())
	}

	// A different Set on the same loader keeps the default HTML escaping.
	setEsc := NewSet(loader)
	tpl2, _ := setEsc.GetTemplate("/p")
	var b2 bytes.Buffer
	if err := tpl2.Execute(&b2, VarMap{"x": rv("<b>")}, nil); err != nil {
		t.Fatal(err)
	}
	if b2.String() != `&lt;b&gt;|<b>` {
		t.Fatalf("escaping choice must not leak between Sets, got %q", b2.String())
	}
}

func TestNameResolution_Autoescape_IncludeDoesNotChangeSetEscape(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/part", `[{{x}}]`)
	loader.Set("/page", `{{include "/part"}}<{{x}}>`)
	rec := NewLookupRecorder()
	set := NewSet(loader, func(s *Set) { s.parseObserver = rec })
	tpl, _ := set.GetTemplate("/page")
	var b bytes.Buffer
	if err := tpl.execute(&b, VarMap{"x": rv("<i>")}, nil, rec); err != nil {
		t.Fatal(err)
	}
	// Both the include and the outer render share the same Set -> same escapee.
	want := `[&lt;i&gt;]<&lt;i&gt;>`
	if b.String() != want {
		t.Fatalf("include must inherit Set escape context, got %q", b.String())
	}
}

// --- run-time errors: identical text with/without observer ------------------

func TestNameResolution_RuntimeErrorTextIdentical(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/a", `{{include "/b"}}`)
	loader.Set("/b", `{{include "/c"}}`)
	loader.Set("/c", `{{missingfn()}}`)

	render := func(observer lookupObserver) (string, error) {
		set := NewSet(loader)
		tpl, err := set.GetTemplate("/a")
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		return b.String(), tpl.execute(&b, nil, nil, observer)
	}

	_, errOff := render(nil)
	_, errOn := render(NewLookupRecorder())
	if errOff == nil || errOn == nil {
		t.Fatal("expected unresolved-identifier error through include chain")
	}
	if errOff.Error() != errOn.Error() {
		t.Fatalf("observer must not change error text:\noff=%q\non =%q", errOff.Error(), errOn.Error())
	}
	if !strings.Contains(errOn.Error(), `("/c":1)`) {
		t.Fatalf("error must locate the deepest include template /c:1: %v", errOn)
	}
	if !strings.Contains(errOn.Error(), `identifier "missingfn" not available`) {
		t.Fatalf("unexpected error text: %v", errOn)
	}
}

func TestNameResolution_ObserverPreservesOutputOnError(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/p", `before|{{bad()}}|after`)

	run := func(observer lookupObserver) (string, string) {
		set := NewSet(loader)
		tpl, _ := set.GetTemplate("/p")
		var b bytes.Buffer
		err := tpl.execute(&b, nil, nil, observer)
		if err == nil {
			t.Fatal("expected error")
		}
		return b.String(), err.Error()
	}
	outOff, errOff := run(nil)
	outOn, errOn := run(NewLookupRecorder())
	if outOff != outOn {
		t.Fatalf("partial output must match: %q vs %q", outOff, outOn)
	}
	if outOff != "before|" {
		t.Fatalf("output before the error must be buffered, got %q", outOff)
	}
	if errOff != errOn {
		t.Fatalf("error text must match: %q vs %q", errOff, errOn)
	}
}

// --- observer event cap -----------------------------------------------------

func TestNameResolution_ObserverEventCapTruncatesButKeepsStack(t *testing.T) {
	loader := NewInMemLoader()
	// one lookup per iteration -> a bounded, deterministic number
	loader.Set("/p", `{{range i := ints(0, .)}}{{g}}{{end}}`)

	rec := NewLookupRecorder()
	rec.EventCap = 3
	set := NewSet(loader, func(s *Set) { s.parseObserver = rec })
	tpl, _ := set.GetTemplate("/p")
	var b bytes.Buffer
	if err := tpl.execute(&b, VarMap{"g": rv("G")}, 10, rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.Events()) != 3 {
		t.Fatalf("retained events must be capped at 3, got %d", len(rec.Events()))
	}
	if !rec.Truncated() {
		t.Fatal("recorder must report truncation")
	}
	stack := rec.LastStack()
	if len(stack) != 1 || stack[0].String() != "execute:/p" {
		t.Fatalf("truncation must preserve template stack, got %v", stack)
	}
}

func TestNameResolution_ObserverCapRetainsStackAcrossInclude(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/part", `{{g}}{{g}}{{g}}{{g}}`)
	loader.Set("/page", `pre{{include "/part"}}`)
	rec := NewLookupRecorder()
	rec.EventCap = 1
	set := NewSet(loader, func(s *Set) { s.parseObserver = rec })
	tpl, _ := set.GetTemplate("/page")
	var b bytes.Buffer
	if err := tpl.execute(&b, VarMap{"g": rv("G")}, nil, rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.Events()) != 1 || !rec.Truncated() {
		t.Fatalf("expected 1 retained event and truncation, got %d trunc=%v", len(rec.Events()), rec.Truncated())
	}
	// The last lookup happened inside the include; its stack survives.
	stack := rec.LastStack()
	if len(stack) != 2 || stack[1].String() != "include:/part" {
		t.Fatalf("last stack should retain the include frame, got %v", stack)
	}
}

// --- observer off: allocation / behaviour parity ----------------------------

func TestNameResolution_ObserverOffDoesNotAllocateOrAlter(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/p", `{{x}}-{{upper("a")}}`)
	set := NewSet(loader)
	tpl, _ := set.GetTemplate("/p")

	run := func(observer lookupObserver) string {
		var b bytes.Buffer
		if err := tpl.execute(&b, VarMap{"x": rv("V")}, nil, observer); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	if off, on := run(nil), run(NewLookupRecorder()); off != on {
		t.Fatalf("observer changed output: %q vs %q", off, on)
	}
	// With the observer off, no lookupEvent/recorder allocation should occur.
	allocs := testing.AllocsPerRun(10, func() {
		var b bytes.Buffer
		if err := tpl.execute(&b, VarMap{"x": rv("V")}, nil, nil); err != nil {
			t.Fatal(err)
		}
	})
	// Characterization guard: the observed render of the same template costs
	// about 21 allocs; the observer-off path must remain on its small baseline
	// (~9), proving the event/candidate slices are never built when disabled.
	if allocs > 12 {
		t.Fatalf("observer-off render allocations regressed (expected ~9): %.1f", allocs)
	}
}
