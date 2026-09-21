package jet

import (
	"bytes"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"testing"
)

// --- template reference cycles ---------------------------------------------
//
// Circular extends/import are resolved lazily *while parsing* and there is no
// parse-time cycle guard, so they recurse until the Go runtime aborts with a
// fatal stack overflow (unrecoverable; it takes the process down). Circular
// include is run time and is bounded by the include-depth guard at 100_000,
// producing an ordinary render error. These are characterization tests: they
// pin the current behaviour rather than promising a fix.

// TestNameResolution_CycleExtends_AndImport_Subprocess exercises fatal parse
// recursion in a child process so a fatal runtime abort cannot fail the parent
// test binary.
func TestNameResolution_CycleExtendsAndImport_Subprocess(t *testing.T) {
	if os.Getenv("JET_CYCLE_CHILD") == "extends" {
		debug.SetMaxStack(1 << 20) // bound the fatal stack-growth time in this throwaway child
		l := NewInMemLoader()
		l.Set("/a", `{{extends "/b"}}A`)
		l.Set("/b", `{{extends "/a"}}B`)
		s := NewSet(l)
		tpl, _ := s.GetTemplate("/a")
		_ = tpl.Execute(&bytes.Buffer{}, nil, nil)
		return
	}
	if os.Getenv("JET_CYCLE_CHILD") == "import" {
		debug.SetMaxStack(1 << 20)
		l := NewInMemLoader()
		l.Set("/a", `{{import "/b"}}{{yield x()}}`)
		l.Set("/b", `{{import "/a"}}{{block x()}}X{{end}}`)
		s := NewSet(l)
		tpl, _ := s.GetTemplate("/a")
		_ = tpl.Execute(&bytes.Buffer{}, nil, nil)
		return
	}

	for _, mode := range []string{"extends", "import"} {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run", "TestNameResolution_CycleExtendsAndImport_Subprocess")
			// Re-create the environment without the parent's child selector so
			// the child runs exactly once and the parent test skips in it.
			env := []string{"JET_CYCLE_CHILD=" + mode, "GOTRACEBACK=none"}
			for _, kv := range os.Environ() {
				if !strings.HasPrefix(kv, "JET_CYCLE_CHILD=") {
					env = append(env, kv)
				}
			}
			cmd.Env = env
			out, _ := cmd.CombinedOutput()
			if !bytes.Contains(out, []byte("stack overflow")) {
				t.Fatalf("expected fatal stack overflow for circular %s, got:\n%s", mode, out)
			}
		})
	}
}

// Pretend the child-mode code above is a real test so the -test.run selector
// matches something in the child process.
func init() {}

func TestNameResolution_CycleIncludeBounded(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/a", `A{{include "/b"}}`)
	loader.Set("/b", `B{{include "/a"}}`)

	set := NewSet(loader)
	tpl, err := set.GetTemplate("/a")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err = tpl.Execute(&buf, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "maximum 'include' depth (100000) exceeded") {
		t.Fatalf("expected include-depth error, got %v", err)
	}
	// Output produced before the failure is retained (100_000 "AB" prefixes).
	if buf.Len() != 100_001 {
		t.Fatalf("expected 100001 bytes of buffered output, got %d", buf.Len())
	}
}

// Truncation with template-stack retention is asserted against a bounded
// include fan (a fixed, small number of include frames) rather than the
// 100_000-deep pathological cycle, which would make an observed render O(n) in
// event construction. The 100_000 guard itself is covered unobserved above.
func TestNameResolution_ObserverStackRetainedOnTruncation(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/a", `{{g}}{{include "/b"}}{{g}}`)
	loader.Set("/b", `{{g}}{{include "/c"}}`)
	loader.Set("/c", `{{g}}{{g}}`)

	rec := NewLookupRecorder()
	rec.EventCap = 2
	set := NewSet(loader, func(s *Set) { s.parseObserver = rec })
	tpl, err := set.GetTemplate("/a")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tpl.execute(&buf, VarMap{"g": rv("G")}, nil, rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.Events()) != 2 || !rec.Truncated() {
		t.Fatalf("expected 2 retained events and truncation, got %d trunc=%v", len(rec.Events()), rec.Truncated())
	}
	// Events retained before the cap already entered the include chain; their
	// stack snapshot survives the truncation (the deeper /c events are dropped).
	stack := rec.LastStack()
	if len(stack) != 2 {
		t.Fatalf("expected execute>/b stack, got %v", stack)
	}
	if stack[0].String() != "execute:/a" || stack[1].String() != "include:/b" {
		t.Fatalf("unexpected retained stack: %v", stack)
	}
}

// --- duplicate import -------------------------------------------------------

func TestNameResolution_DuplicateImportSamePathCachedOnce(t *testing.T) {
	loader := &countingLoader{inner: NewInMemLoader()}
	loader.inner.Set("/lib.jet", `{{block m()}}M{{end}}`)
	loader.inner.Set("/page.jet", `{{import "/lib.jet"}}{{import "/lib.jet"}}{{yield m()}}`)

	set := NewSet(loader)
	tpl, err := set.GetTemplate("/page.jet")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, nil, nil); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "M" {
		t.Fatalf("got %q", buf.String())
	}
	// /lib.jet is parsed once: the second import is a cache hit (same object),
	// so Exists is hit during the first parse only for that template.
	if loader.existsCalls("/lib.jet") != 1 {
		t.Fatalf("expected exactly one loader Exists for /lib.jet, got %d", loader.existsCalls("/lib.jet"))
	}
	if tpl.imports[0] != tpl.imports[1] {
		t.Fatal("duplicate imports must resolve to the same cached *Template")
	}
}

// --- loader returns different contents for the same path --------------------

func TestNameResolution_LoaderDrift_CacheHoldsStable(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/p", `V1`)

	set := NewSet(loader)
	first, err := set.GetTemplate("/p")
	if err != nil {
		t.Fatal(err)
	}
	loader.Set("/p", `V2`) // loader now reports different bytes for same path
	second, err := set.GetTemplate("/p")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("production Set must keep serving the cached template on drift")
	}
	var b1, b2 bytes.Buffer
	if err := first.Execute(&b1, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := second.Execute(&b2, nil, nil); err != nil {
		t.Fatal(err)
	}
	if b1.String() != "V1" || b2.String() != "V1" {
		t.Fatalf("cached render must stay V1, got %q/%q", b1.String(), b2.String())
	}
}

func TestNameResolution_LoaderDrift_DevelopmentModeReloads(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/p", `V1`)
	set := NewSet(loader, InDevelopmentMode())
	first, _ := set.GetTemplate("/p")
	loader.Set("/p", `V2`)
	second, _ := set.GetTemplate("/p")
	if first == second {
		t.Fatal("development mode must bypass the cache and reload")
	}
	var b bytes.Buffer
	if err := second.Execute(&b, nil, nil); err != nil {
		t.Fatal(err)
	}
	if b.String() != "V2" {
		t.Fatalf("development mode should see new contents, got %q", b.String())
	}
}

// --- cache hit vs cold load: identical resolution source --------------------

func TestNameResolution_CacheHitAndColdLoadResolveIdentically(t *testing.T) {
	loader := NewInMemLoader()
	loader.Set("/lib.jet", `{{block m()}}<{{g}}>{{end}}`)
	loader.Set("/page.jet", `{{import "/lib.jet"}}{{yield m()}}`)

	render := func(set *Set, rec *LookupRecorder, entry string) string {
		tpl, err := set.GetTemplate(entry)
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err := tpl.execute(&b, VarMap{"g": rv("GV")}, nil, rec); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}

	// Cold load with observer.
	recCold := NewLookupRecorder()
	setCold := NewSet(loader, func(s *Set) { s.parseObserver = recCold })
	coldOut := render(setCold, recCold, "/page.jet")

	// Warm cache, same Set.
	recWarm := NewLookupRecorder()
	warmOut := render(setCold, recWarm, "/page.jet")

	// Another Set over the same loader = fully cold again.
	recCold2 := NewLookupRecorder()
	setCold2 := NewSet(loader, func(s *Set) { s.parseObserver = recCold2 })
	cold2Out := render(setCold2, recCold2, "/page.jet")

	if coldOut != "<GV>" || warmOut != coldOut || cold2Out != coldOut {
		t.Fatalf("cache hit must not change rendered source: cold=%q warm=%q cold2=%q", coldOut, warmOut, cold2Out)
	}

	// Run-time decisive paths must be identical warm vs cold.
	blockCold := recCold.findEvent(lookupBlock, "m")
	blockWarm := recWarm.findEvent(lookupBlock, "m")
	if blockCold == nil || blockWarm == nil {
		t.Fatal("missing block lookup")
	}
	if blockCold.SelectedTemplate != "/lib.jet" || blockCold.SelectedTemplate != blockWarm.SelectedTemplate {
		t.Fatalf("cache hit changed block source: cold=%s warm=%s", blockCold.SelectedTemplate, blockWarm.SelectedTemplate)
	}
	gCold := recCold.findEvent(lookupVariable, "g")
	gWarm := recWarm.findEvent(lookupVariable, "g")
	if gCold.Selected != gWarm.Selected || gCold.SelectedDepth != gWarm.SelectedDepth {
		t.Fatalf("cache hit changed variable path: cold=%s warm=%s", gCold.Describe(), gWarm.Describe())
	}
}
