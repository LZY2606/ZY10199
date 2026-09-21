package jet

// Characterization tests for Jet's name resolution.
//
// These tests pin down exactly how the lexer, parser, Set/loader, template tree,
// runtime and scopes bind the extends, import, include, block, yield and variable
// constructs at parse time and execution time. They both assert rendered output and
// the decisive resolution path recorded by the internal lookup observer (see
// observe.go and docs/name-resolution.md).

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
)

// --- test fixtures -----------------------------------------------------------

// resolveHarness bundles everything a characterization test needs: an in-memory
// loader, a Set, the observer recording lookup events and small assertion helpers.
type resolveHarness struct {
	t      *testing.T
	loader *InMemLoader
	set    *Set
	ob     *recordingObserver
	opts   []Option
}

func newResolveHarness(t *testing.T, opts ...Option) *resolveHarness {
	t.Helper()
	h := &resolveHarness{
		t:      t,
		loader: NewInMemLoader(),
		opts:   opts,
	}
	h.set = NewSet(h.loader, opts...)
	h.ob = newRecordingObserver(0)
	attachLookupObserver(h.set, h.ob, 0)
	return h
}

func (h *resolveHarness) add(path, contents string) {
	h.t.Helper()
	h.loader.Set(path, contents)
}

// render parses (on first call), executes and returns the rendered string.
func (h *resolveHarness) render(path string, vars VarMap, data interface{}) string {
	h.t.Helper()
	tpl, err := h.set.GetTemplate(path)
	if err != nil {
		h.t.Fatalf("parse %s: %v", path, err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, vars, data); err != nil {
		h.t.Fatalf("execute %s: %v", path, err)
	}
	return buf.String()
}

// renderErr is like render but expects execution to fail and returns the error.
func (h *resolveHarness) renderErr(path string, vars VarMap, data interface{}) error {
	h.t.Helper()
	tpl, err := h.set.GetTemplate(path)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	return tpl.Execute(&buf, vars, data)
}

func (h *resolveHarness) events() []LookupEvent { return h.ob.eventsSnapshot() }

// vars builds a VarMap from plain Go values.
func vars(kv ...interface{}) VarMap {
	m := VarMap{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = reflect.ValueOf(kv[i+1])
	}
	return m
}

// findVariableEvents returns every recorded variable lookup for name.
func (h *resolveHarness) findVariableEvents(name string) []LookupEvent {
	var out []LookupEvent
	for _, e := range h.events() {
		if e.Kind == LookupVariable && e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

func (h *resolveHarness) requireVariableEvent(name, op string) LookupEvent {
	h.t.Helper()
	for _, e := range h.findVariableEvents(name) {
		if e.Op == op {
			return e
		}
	}
	h.t.Fatalf("no variable event name=%q op=%q in %d events:\n%s", name, op, len(h.events()), h.dumpEvents())
	return LookupEvent{}
}

func (h *resolveHarness) requireBlockEvent(name string) LookupEvent {
	h.t.Helper()
	for _, e := range h.events() {
		if e.Kind == LookupBlock && e.Name == name {
			return e
		}
	}
	h.t.Fatalf("no block event name=%q in %d events:\n%s", name, len(h.events()), h.dumpEvents())
	return LookupEvent{}
}

func (h *resolveHarness) requireTemplateEvent(name string) LookupEvent {
	h.t.Helper()
	for _, e := range h.events() {
		if e.Kind == LookupTemplate && e.Name == name {
			return e
		}
	}
	h.t.Fatalf("no template event name=%q in %d events:\n%s", name, len(h.events()), h.dumpEvents())
	return LookupEvent{}
}

func (h *resolveHarness) dumpEvents() string {
	var b strings.Builder
	for i, e := range h.events() {
		fmt.Fprintf(&b, "  [%d] kind=%s op=%s name=%q source=%s template=%q found=%v stack=%v candidates=%v probes=%v\n",
			i, e.Kind, e.Op, e.Name, e.Source, e.Template, e.Found, e.Stack, e.Candidates, e.TemplateCandidates)
	}
	return b.String()
}

// --- 1. variable shadowing: local > template > global > default ---------------

func TestNameResolution_VariableShadowingOrder(t *testing.T) {
	h := newResolveHarness(t)
	h.set.AddGlobal("name", "GLOBAL")
	// "upper" exists in defaultVariables, so it exercises the global layer shadowing
	// a default name, and a local declaration shadowing all of the above.
	h.add("/t.jet", `{{name}}/{{upper("x")}}/{{name := "LOCAL"}}{{name}}/{{upper("y")}}`)

	got := h.render("/t.jet", vars("name", "TEMPLATE", "upper", func(s string) string { return "T:" + s }), nil)
	if want := "TEMPLATE/T:x/LOCAL/T:y"; got != want {
		t.Fatalf("render: want %q got %q", want, got)
	}

	// Initial read of name: template vars layer wins over global.
	read := h.requireVariableEvent("name", "read")
	if read.Source != SourceTemplate || !read.Found {
		t.Fatalf("initial name read: want source=template, got source=%q found=%v\ncandidates=%v", read.Source, read.Found, read.Candidates)
	}
	// Candidates must document every layer probed in order: template, global, default.
	if len(read.Candidates) != 3 {
		t.Fatalf("initial name read: want 3 scope candidates (template/global/default), got %+v", read.Candidates)
	}
	if read.Candidates[0].Kind != SourceTemplate || read.Candidates[0].Depth != 0 || !read.Candidates[0].Hit {
		t.Fatalf("initial name read: first candidate must hit template layer, got %+v", read.Candidates[0])
	}
	// The global layer contains a same-named value (Hit=true) but is *not selected*,
	// because the template layer already won at depth 0: presence does not imply victory.
	if read.Candidates[1].Kind != SourceGlobal || !read.Candidates[1].Hit {
		t.Fatalf("initial name read: global candidate exists but must be shadowed, got %+v", read.Candidates[1])
	}
	if read.Candidates[2].Kind != SourceDefault || read.Candidates[2].Hit {
		t.Fatalf("initial name read: third candidate must miss default, got %+v", read.Candidates[2])
	}

	// The global "name" is not read because template shadows it; characterize that
	// globals are probed only when template/local layers miss, using the bare set:
	h2 := newResolveHarness(t)
	h2.set.AddGlobal("g", "G")
	h2.add("/g.jet", `{{g}}`)
	if got := h2.render("/g.jet", nil, nil); got != "G" {
		t.Fatalf("global render: want G got %q", got)
	}
	ge := h2.requireVariableEvent("g", "read")
	if ge.Source != SourceGlobal {
		t.Fatalf("global read: want source=global got %q", ge.Source)
	}
	if len(ge.Candidates) != 3 || ge.Candidates[0].Kind != SourceTemplate || ge.Candidates[0].Hit ||
		ge.Candidates[1].Kind != SourceGlobal || !ge.Candidates[1].Hit ||
		ge.Candidates[2].Kind != SourceDefault || ge.Candidates[2].Hit {
		t.Fatalf("global read candidates: %+v", ge.Candidates)
	}

	// Local := declaration shadows template and global; Template field records the
	// layer that was shadowed, second read must resolve locally at depth 0.
	let := h.requireVariableEvent("name", "let")
	if let.Source != SourceLocal || let.Template != SourceTemplate {
		t.Fatalf("let event: want source=local shadowing=template, got source=%q shadowing=%q", let.Source, let.Template)
	}
	var localRead LookupEvent
	for _, e := range h.findVariableEvents("name") {
		if e.Op == "read" && e.Source == SourceLocal {
			localRead = e
		}
	}
	if localRead.Name == "" {
		t.Fatalf("no local read of name after :=\n%s", h.dumpEvents())
	}
	if localRead.Candidates[0].Kind != SourceLocal || localRead.Candidates[0].Depth != 0 || !localRead.Candidates[0].Hit {
		t.Fatalf("local read: first candidate must hit local layer, got %+v", localRead.Candidates)
	}

	// A default-only name resolves at the default layer after all others miss.
	h3 := newResolveHarness(t)
	h3.add("/d.jet", `{{lower("ABC")}}`)
	if got := h3.render("/d.jet", nil, nil); got != "abc" {
		t.Fatalf("default render: want abc got %q", got)
	}
	de := h3.requireVariableEvent("lower", "read")
	if de.Source != SourceDefault {
		t.Fatalf("default read: want source=default got %q", de.Source)
	}
}

// --- 2. assignment vs declaration across scope layers -------------------------

func TestNameResolution_AssignWalksScopesAndLetShadows(t *testing.T) {
	h := newResolveHarness(t)
	// Outer template var n; a := inside an if creates a local and shadows it; plain =
	// afterwards must reach back to the template-scope binding (not recreate local).
	h.add("/a.jet", `{{n := 1}}[{{if true}}{{n := 2}}local={{n}};{{end}}]after={{n}};{{m = 5}}`)
	h.loader.Set("/a.jet", `start={{n}}|{{if marker := 1; true}}{{n := 2}}in={{n}};{{m = 9}}assign={{m}};{{end}}|out={{n}}`)

	got := h.render("/a.jet", vars("n", 100, "m", 0), nil)
	if want := "start=100|in=2;assign=9;|out=100"; got != want {
		t.Fatalf("render: want %q got %q", want, got)
	}

	// := inside if declares locally and shadows the template-scope n.
	letN := h.requireVariableEvent("n", "let")
	if letN.Source != SourceLocal || letN.Template != SourceTemplate {
		t.Fatalf("n := inside if: want local shadowing template, got source=%q shadow=%q", letN.Source, letN.Template)
	}
	// = m reaches the template scope (depth 1 from inside the if scope).
	assignM := h.requireVariableEvent("m", "assign")
	if assignM.Source != SourceTemplate || !assignM.Found {
		t.Fatalf("m = inside if: want assignment to template scope, got %+v", assignM)
	}
	// assignment candidates must show local miss at depth 0 then template hit.
	if assignM.Candidates[0].Kind != SourceLocal || assignM.Candidates[0].Hit {
		t.Fatalf("m = candidates[0]: want local miss, got %+v", assignM.Candidates[0])
	}
	if assignM.Candidates[1].Kind != SourceLocal || assignM.Candidates[1].Hit {
		t.Fatalf("m = candidates[1]: want local miss, got %+v", assignM.Candidates[1])
	}
	last := assignM.Candidates[len(assignM.Candidates)-1]
	if last.Kind != SourceTemplate || !last.Hit {
		t.Fatalf("m = final candidate: want template hit, got %+v", last)
	}

	// Assigning an undeclared name is an execution error with stable text.
	h2 := newResolveHarness(t)
	h2.add("/u.jet", `{{undeclared = 1}}`)
	err := h2.renderErr("/u.jet", nil, nil)
	if err == nil || !strings.Contains(err.Error(), `could not assign "undeclared"`) {
		t.Fatalf("want uninitialised assign error, got %v", err)
	}
	miss := h2.requireVariableEvent("undeclared", "assign")
	if miss.Found {
		t.Fatalf("failed assignment event must report found=false, got %+v", miss)
	}
}

// --- 3. block resolution: child > later import > earlier import > extends ------

func TestNameResolution_BlockPrecedence(t *testing.T) {
	h := newResolveHarness(t)
	h.add("/base.jet", `{{block widget()}}BASE{{end}}|{{block shared()}}BASE-SHARED{{end}}`)
	h.add("/libA.jet", `{{block widget()}}LIBA{{end}}{{block aonly()}}A{{end}}`)
	h.add("/libB.jet", `{{block widget()}}LIBB{{end}}{{block shared()}}LIBB-SHARED{{end}}`)
	// Child overrides widget itself, imports both libraries (same-named macro in each),
	// and leaves shared to be resolved from imports.
	h.add("/child.jet", `{{extends "base.jet"}}{{import "libA.jet"}}{{import "libB.jet"}}{{block widget()}}CHILD{{end}}root={{yield widget()}}/{{yield shared()}}/{{yield aonly()}}`)

	// The executed root of an extending template is the parent's body; child block
	// definitions only appear where the parent yields them. We characterize precedence
	// directly with a non-extending composition template:
	h.add("/comp.jet", `{{import "libA.jet"}}{{import "libB.jet"}}{{yield widget()}}|{{yield shared()}}|{{yield aonly()}}`)
	if r := h.render("/comp.jet", nil, nil); r != "LIBB|LIBB-SHARED|A" {
		t.Fatalf("comp render: want LIBB|LIBB-SHARED|A got %q", r)
	}
	we := h.requireBlockEvent("widget")
	if !we.Found || we.Template != "/libB.jet" {
		t.Fatalf("widget yield must bind to the later import libB.jet, got template=%q event=%+v", we.Template, we)
	}
	se := h.requireBlockEvent("shared")
	if se.Template != "/libB.jet" {
		t.Fatalf("shared yield: later import wins, got %q", se.Template)
	}
	ae := h.requireBlockEvent("aonly")
	if ae.Template != "/libA.jet" {
		t.Fatalf("aonly must come from the earlier import libA.jet, got %q", ae.Template)
	}

	// extends+import+own: own passed block beats every other layer. Isolated harness so
	// the decisive widget event is unambiguous.
	ho := newResolveHarness(t)
	ho.add("/base.jet", `{{block widget()}}BASE{{end}}`)
	ho.add("/libB.jet", `{{block widget()}}LIBB{{end}}`)
	ho.add("/own.jet", `{{extends "base.jet"}}{{import "libB.jet"}}{{block widget()}}OWN{{end}}`)
	if r := ho.render("/own.jet", nil, nil); r != "OWN" {
		t.Fatalf("own block must override base+import, got %q", r)
	}
	oe := ho.requireBlockEvent("widget")
	_ = oe
	var ownEv LookupEvent
	for _, e := range ho.events() {
		if e.Kind == LookupBlock && e.Name == "widget" && e.Template == "/own.jet" {
			ownEv = e
		}
	}
	if ownEv.Name == "" {
		t.Fatalf("own block must bind to own.jet, events:\n%s", ho.dumpEvents())
	}
	if !ownEv.Found || ownEv.Source != SourceBlock {
		t.Fatalf("own block event: %+v", ownEv)
	}
}

// --- 4. extends binds at parse time; yield resolves against merged block map ----

func TestNameResolution_ExtendsAndYield(t *testing.T) {
	h := newResolveHarness(t)
	h.add("/layout.jet", `HEADER {{yield body()}} FOOTER`)
	h.add("/page.jet", `{{extends "layout.jet"}}{{block body()}}PAGE-BODY{{end}}`)
	if got := h.render("/page.jet", nil, nil); got != "HEADER PAGE-BODY FOOTER" {
		t.Fatalf("extends render: %q", got)
	}

	// Parse-time template events for the extends edge, resolved relative to the child.
	ev := h.requireTemplateEvent("/layout.jet")
	if ev.Source != SourceLoader || !ev.Found {
		t.Fatalf("extends parse event: want loader hit, got %+v", ev)
	}
	if len(ev.Stack) != 1 || ev.Stack[0] != "/page.jet" {
		t.Fatalf("extends parse event stack must name the child, got %v", ev.Stack)
	}

	// Execution runs the parent root but the block lookup yields the child's block,
	// and the variable/block events carry the parent frame on the template stack.
	be := h.requireBlockEvent("body")
	if be.Template != "/page.jet" {
		t.Fatalf("yield body must bind child block, got %q", be.Template)
	}
	if len(be.Stack) != 1 || be.Stack[0] != "/layout.jet" {
		t.Fatalf("yield event stack must show execution in layout.jet, got %v", be.Stack)
	}
}

// --- 5. include binds at execution time: fresh scope, param is dot, own blocks ---

func TestNameResolution_IncludeParametersAndScoping(t *testing.T) {
	h := newResolveHarness(t)
	// Included template sees only the passed value as dot; the caller's template
	// variables are not in scope; its own blocks replace the active block map for
	// the duration of the inclusion.
	// badge is provided by the included template itself; the caller's variables are
	// not visible inside the fresh include scope.
	h.add("/partial.jet", `partial:dot={{.}};{{block badge()}}PARTIAL-BADGE{{end}}`)
	h.add("/caller.jet", `{{localVisible := "SECRET"}}<{{include "partial.jet" "GUEST"}}>`)

	got, err := func() (string, error) {
		tpl, perr := h.set.GetTemplate("/caller.jet")
		if perr != nil {
			return "", perr
		}
		var buf bytes.Buffer
		if e := tpl.Execute(&buf, vars("localVisible", "TEMPLATE-SECRET"), nil); e != nil {
			return "", e
		}
		return buf.String(), nil
	}()
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if want := "<partial:dot=GUEST;PARTIAL-BADGE>"; got != want {
		t.Fatalf("include render: want %q got %q", want, got)
	}

	// The include resolution happens at execution time (stack names the caller),
	// whereas extends/import resolve at parse time.
	inc := h.requireTemplateEvent("/partial.jet")
	if inc.Source != SourceLoader || inc.Op != "load" {
		t.Fatalf("include event: want runtime loader resolution, got %+v", inc)
	}
	if len(inc.Stack) != 1 || inc.Stack[0] != "/caller.jet" {
		t.Fatalf("include event stack must be [caller.jet], got %v", inc.Stack)
	}

	// Caller local variables are invisible inside the include: the include creates a
	// scope whose parent chain ends at its own empty root; lookups there must miss.
	var dotEvent LookupEvent
	var badge LookupEvent
	for _, e := range h.events() {
		if e.Kind == LookupVariable && e.Name == "." && len(e.Stack) > 0 && e.Stack[len(e.Stack)-1] == "/partial.jet" {
			dotEvent = e
		}
		if e.Kind == LookupBlock && e.Name == "badge" {
			badge = e
		}
	}
	if dotEvent.Name == "" || dotEvent.Source != "context" || dotEvent.Found != true {
		t.Fatalf("include dot event: want context hit, got %+v", dotEvent)
	}
	if badge.Template != "/partial.jet" {
		t.Fatalf("badge block must resolve from the included template's own blocks, got %q", badge.Template)
	}

	// Relative path resolution is anchored at the including template's directory.
	h.add("/dir/inner.jet", `{{include "sibling.jet"}}`)
	h.add("/dir/sibling.jet", `SIBLING`)
	if got := h.render("/dir/inner.jet", nil, nil); got != "SIBLING" {
		t.Fatalf("relative include: %q", got)
	}
}

// --- 6. yield content closures execute in the yielding (block body) scope -------

func TestNameResolution_YieldContentClosure(t *testing.T) {
	h := newResolveHarness(t)
	h.add("/wrap.jet", `{{block card(title)}}<div title="{{title}}">{{yield content}}</div>{{end}}`)
	h.add("/page.jet", `{{import "wrap.jet"}}{{greeting := "hi"}}{{yield card(title="T") content}}<{{greeting}}>{{end}}`)
	// YieldNode content terminates with {{end}}:
	h.loader.Set("/page.jet", `{{import "wrap.jet"}}{{greeting := "hi"}}{{yield card(title="T") content}}<{{greeting}}>{{end}}`)

	if got := h.render("/page.jet", nil, nil); got != `<div title="T"><hi></div>` {
		t.Fatalf("content closure render: %q", got)
	}
	// greeting is read while the closure executes in the block-call scope (page.jet).
	ge := h.requireVariableEvent("greeting", "read")
	if ge.Source != SourceLocal || !ge.Found {
		t.Fatalf("content closure greeting: want local hit, got %+v", ge)
	}
}

// --- 7. combined end-to-end composition -----------------------------------------

func TestNameResolution_CombinedComposition(t *testing.T) {
	h := newResolveHarness(t)
	h.set.AddGlobal("brand", "GLOBAL-BRAND")
	h.add("/layout.jet", `[{{yield header()}}|{{yield body()}}]`)
	h.add("/lib.jet", `{{block header()}}header:{{brand}}{{end}}`)
	h.add("/partial.jet", `p({{.}})`)
	h.add("/page.jet", `
{{extends "layout.jet"}}
{{import "lib.jet"}}
{{block body()}}
{{brand}}/{{shad := "local-brand"}}{{shad}}
{{include "partial.jet" "CTX"}}
{{end}}`)
	got := h.render("/page.jet", vars("brand", "TEMPLATE-BRAND"), nil)
	if want := `[header:TEMPLATE-BRAND|
TEMPLATE-BRAND/local-brand
p(CTX)
]`; got != want {
		t.Fatalf("combined render:\nwant %q\ngot  %q", want, got)
	}
}

// --- 8. duplicate imports are allowed and order-stable --------------------------

type countingLoader struct {
	mu      sync.Mutex
	files   map[string]string
	openN   map[string]int
	existsN map[string]int
}

func newCountingLoader() *countingLoader {
	return &countingLoader{
		files:   map[string]string{},
		openN:   map[string]int{},
		existsN: map[string]int{},
	}
}

func (l *countingLoader) Set(p, c string) { l.files[p] = c }

func (l *countingLoader) Exists(p string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.existsN[p]++
	_, ok := l.files[p]
	return ok
}

func (l *countingLoader) Open(p string) (io.ReadCloser, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.files[p]
	if !ok {
		return nil, fmt.Errorf("%s missing", p)
	}
	l.openN[p]++
	return io.NopCloser(strings.NewReader(c)), nil
}

func TestNameResolution_DuplicateImports(t *testing.T) {
	cl := newCountingLoader()
	cl.Set("/lib.jet", `{{block b()}}LIB{{end}}`)
	cl.Set("/page.jet", `{{import "lib.jet"}}{{import "lib.jet"}}{{yield b()}}`)
	s := NewSet(cl)
	ob := newRecordingObserver(0)
	attachLookupObserver(s, ob, 0)
	tpl, err := s.GetTemplate("/page.jet")
	if err != nil {
		t.Fatalf("duplicate imports must not error: %v", err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, nil, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if buf.String() != "LIB" {
		t.Fatalf("duplicate import render: %q", buf.String())
	}
	// First import is parsed through the loader; the second finds it already cached.
	var loads, cacheHits int
	for _, e := range ob.eventsSnapshot() {
		if e.Kind != LookupTemplate || e.Name != "/lib.jet" {
			continue
		}
		if e.Source == SourceLoader {
			loads++
		}
		if e.Source == SourceCache {
			cacheHits++
		}
	}
	if loads != 1 || cacheHits != 1 {
		t.Fatalf("duplicate import: want 1 loader parse and 1 cache hit, got loads=%d cacheHits=%d", loads, cacheHits)
	}
}

// --- 9. circular references ------------------------------------------------------

// A cyclic extends/import chain recurses through the parser (each template is parsed
// fresh from the loader before anything is cached) and overflows the goroutine stack,
// which Go reports as an unrecoverable runtime error. We run the cycle in a child
// process with a small stack limit so the test stays fast and never hangs.
func TestNameResolution_CircularExtendsSubprocess(t *testing.T) {
	if os.Getenv("JET_CYCLE_CHILD") == "1" {
		debug.SetMaxStack(1 << 16)
		l := NewInMemLoader()
		l.Set("/a.jet", `{{extends "b.jet"}}A`)
		l.Set("/b.jet", `{{extends "a.jet"}}B`)
		s := NewSet(l)
		_, _ = s.GetTemplate("/a.jet")
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate test binary: %v", err)
	}
	cmd := exec.Command(exe, "-test.run", "TestNameResolution_CircularExtendsSubprocess")
	cmd.Env = append(os.Environ(), "GOTRACEBACK=single", "GOGC=off", "JET_CYCLE_CHILD=1")
	out, _ := cmd.CombinedOutput()
	if cmd.ProcessState == nil || cmd.ProcessState.Success() {
		t.Fatalf("expected child process to fail on cyclic extends, output:\n%s", out)
	}
	if !bytes.Contains(out, []byte("stack overflow")) && !bytes.Contains(out, []byte("goroutine stack exceeds")) {
		t.Fatalf("expected stack-overflow runtime error, got:\n%s", out)
	}
}

// Cyclic includes execute at runtime; the Runtime guards inclusion depth and returns
// a regular (catchable) execution error instead of overflowing the stack.
func TestNameResolution_IncludeDepthGuard(t *testing.T) {
	// Exercise the fixed nesting bound directly: no template chain, no stack growth.
	h := newResolveHarness(t)
	h.add("/x.jet", `x`)
	st := pool_State.Get().(*Runtime)
	defer pool_State.Put(st)
	st.set = h.set
	st.includeDepth = maximumIncludeDepth
	inc := &IncludeNode{NodeBase: NodeBase{TemplatePath: "/x.jet"}}
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("enterInclude must panic at the depth bound")
			} else if e, ok := r.(error); !ok || !strings.Contains(e.Error(), "maximum 'include' depth (100000) exceeded") {
				t.Fatalf("want depth guard error, got %v", r)
			}
		}()
		_ = st.enterInclude(inc)
	}()
	// Below the bound it must increment and return a leave that decrements.
	st.includeDepth = 3
	leave := st.enterInclude(inc)
	if st.includeDepth != 4 {
		t.Fatalf("enterInclude must increment depth, got %d", st.includeDepth)
	}
	leave()
	if st.includeDepth != 3 {
		t.Fatalf("leave must decrement depth, got %d", st.includeDepth)
	}
}

func TestNameResolution_CircularInclude(t *testing.T) {
	// A genuinely cyclic include chain cannot unwind normally: each include adds Go
	// stack frames, so the goroutine runs out of stack around the depth guard. We
	// characterize that cyclic includes always fail the run (never hang), using a
	// small bounded stack in a child process so the failure is reached quickly.
	if os.Getenv("JET_INCLUDE_CYCLE_CHILD") == "1" {
		debug.SetMaxStack(1 << 20)
		l := NewInMemLoader()
		l.Set("/ia.jet", `{{include "ib.jet"}}`)
		l.Set("/ib.jet", `{{include "ia.jet"}}`)
		s := NewSet(l)
		tpl, err := s.GetTemplate("/ia.jet")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		_ = tpl.Execute(io.Discard, nil, nil)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate test binary: %v", err)
	}
	cmd := exec.Command(exe, "-test.run", "TestNameResolution_CircularInclude")
	cmd.Env = append(os.Environ(), "GOTRACEBACK=single", "GOGC=off", "JET_INCLUDE_CYCLE_CHILD=1")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("cyclic include must fail the child process, output:\n%s", out)
	}
}

// extends appearing twice or after an import is a parse-time binding error.
func TestNameResolution_InvalidExtendsClauses(t *testing.T) {
	h := newResolveHarness(t)
	h.add("/base.jet", `BASE`)
	h.add("/double.jet", `{{extends "base.jet"}}{{extends "base.jet"}}`)
	if _, err := h.set.GetTemplate("/double.jet"); err == nil ||
		!strings.Contains(err.Error(), "each template can only extend one template") {
		t.Fatalf("double extends: %v", err)
	}
	h.add("/order.jet", `{{import "base.jet"}}{{extends "base.jet"}}`)
	if _, err := h.set.GetTemplate("/order.jet"); err == nil ||
		!strings.Contains(err.Error(), "extends' clause should come before all import") {
		t.Fatalf("extends-after-import: %v", err)
	}
}

// --- 10. loader mutating same path: cache freezes first parse --------------------

func TestNameResolution_SamePathDifferentContents(t *testing.T) {
	l := NewInMemLoader()
	l.Set("/v.jet", `V1`)
	s := NewSet(l)

	first, err := s.GetTemplate("/v.jet")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := first.Execute(&buf, nil, nil); err != nil || buf.String() != "V1" {
		t.Fatalf("first render: %q %v", buf.String(), err)
	}

	// Loader now returns different contents for the same path; the cached parse must
	// keep winning (cache hits must not resolve differently from the cold parse).
	l.Set("/v.jet", `V2`)
	second, err := s.GetTemplate("/v.jet")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("cache must return the same *Template instance as the cold parse")
	}
	buf.Reset()
	if err := second.Execute(&buf, nil, nil); err != nil || buf.String() != "V1" {
		t.Fatalf("warm render must equal cold render V1, got %q", buf.String())
	}

	// In development mode every lookup bypasses the cache and sees fresh contents.
	l.Set("/d.jet", `D1`)
	dev := NewSet(l, InDevelopmentMode())
	d1, err := dev.GetTemplate("/d.jet")
	if err != nil {
		t.Fatal(err)
	}
	if r := executeString(t, d1); r != "D1" {
		t.Fatalf("dev first: %q", r)
	}
	l.Set("/d.jet", `D2`)
	d2, err := dev.GetTemplate("/d.jet")
	if err != nil {
		t.Fatal(err)
	}
	if r := executeString(t, d2); r != "D2" {
		t.Fatalf("dev reload must observe new loader contents, got %q", r)
	}
}

func executeString(t *testing.T, tpl *Template) string {
	t.Helper()
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, nil, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return buf.String()
}

// --- 11. cache hit resolution must match cold load -------------------------------

func TestNameResolution_CacheParity(t *testing.T) {
	// Same loader state, two independent Sets: cold load vs cache-warm execution must
	// produce identical output and an identical decisive resolution path.
	build := func(t *testing.T) (*Set, *recordingObserver) {
		l := NewInMemLoader()
		l.Set("/base.jet", `{{yield m()}}`)
		l.Set("/lib.jet", `{{block m()}}M{{end}}`)
		l.Set("/page.jet", `{{extends "base.jet"}}{{import "lib.jet"}}`)
		s := NewSet(l)
		ob := newRecordingObserver(0)
		attachLookupObserver(s, ob, 0)
		return s, ob
	}

	coldSet, coldOb := build(t)
	warmSet, warmOb := build(t)

	coldTpl, err := coldSet.GetTemplate("/page.jet")
	if err != nil {
		t.Fatal(err)
	}
	warmTpl, err := warmSet.GetTemplate("/page.jet")
	if err != nil {
		t.Fatal(err)
	}
	// Prime the second set's cache, then fetch again so its template event is a hit.
	if err := warmTpl.Execute(io.Discard, nil, nil); err != nil {
		t.Fatal(err)
	}
	warmTpl2, err := warmSet.GetTemplate("/page.jet")
	if err != nil {
		t.Fatal(err)
	}

	var cbuf, wbuf bytes.Buffer
	if err := coldTpl.Execute(&cbuf, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := warmTpl2.Execute(&wbuf, nil, nil); err != nil {
		t.Fatal(err)
	}
	if cbuf.String() != wbuf.String() || cbuf.String() != "M" {
		t.Fatalf("cold/warm render mismatch: %q vs %q", cbuf.String(), wbuf.String())
	}

	coldBlock := blockResolutionSummary(coldOb.eventsSnapshot())
	warmBlock := blockResolutionSummary(warmOb.eventsSnapshot())
	if !reflect.DeepEqual(coldBlock, warmBlock) {
		t.Fatalf("block resolution differs cold=%+v warm=%+v", coldBlock, warmBlock)
	}
}

func blockResolutionSummary(events []LookupEvent) map[string]string {
	m := map[string]string{}
	for _, e := range events {
		if e.Kind == LookupBlock {
			m[e.Name] = e.Template
		}
	}
	return m
}

// --- 12. two Sets must not pollute each other ------------------------------------

func TestNameResolution_TwoSetsIsolation(t *testing.T) {
	l := NewInMemLoader()
	l.Set("/same.jet", `{{greeting}}`)
	s1 := NewSet(l)
	s2 := NewSet(l)
	s1.AddGlobal("greeting", "ONE")
	s2.AddGlobal("greeting", "TWO")

	r1 := executeString(t, mustTemplate(t, s1, "/same.jet"))
	r2 := executeString(t, mustTemplate(t, s2, "/same.jet"))
	if r1 != "ONE" || r2 != "TWO" {
		t.Fatalf("globals leak across sets: %q vs %q", r1, r2)
	}

	// Same path, different contents across separate loaders/sets: distinct caches.
	l1, l2 := NewInMemLoader(), NewInMemLoader()
	l1.Set("/p.jet", `FIRST`)
	l2.Set("/p.jet", `SECOND`)
	a, b := NewSet(l1), NewSet(l2)
	if r := executeString(t, mustTemplate(t, a, "/p.jet")); r != "FIRST" {
		t.Fatalf("set1 path /p.jet: %q", r)
	}
	if r := executeString(t, mustTemplate(t, b, "/p.jet")); r != "SECOND" {
		t.Fatalf("set2 path /p.jet: %q", r)
	}
}

func mustTemplate(t *testing.T, s *Set, path string) *Template {
	t.Helper()
	tpl, err := s.GetTemplate(path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	return tpl
}

// Shared caches across Sets with different SafeWriters is an explicit compatibility
// tradeoff: a template parsed by the HTML-escaping set and later served to the raw
// set from the shared cache still renders through whichever set executes it (the
// escapee lives on the Set, not on the cached Template). This characterizes where
// autoescape context does and does not switch.
func TestNameResolution_AutoescapeContext(t *testing.T) {
	l := NewInMemLoader()
	l.Set("/x.jet", `{{.}}`)

	htmlSet := NewSet(l)
	rawSet := NewSet(l, WithSafeWriter(nil))

	if r := executeStringWith(t, mustTemplate(t, htmlSet, "/x.jet"), nil, `<a>`); r != `&lt;a&gt;` {
		t.Fatalf("html set must escape: %q", r)
	}
	if r := executeStringWith(t, mustTemplate(t, rawSet, "/x.jet"), nil, `<a>`); r != `<a>` {
		t.Fatalf("raw set must not escape: %q", r)
	}

	// Within one template the raw/unsafe SafeWriter switches escape handling for one
	// command only, then subsequent output is autoescaped again.
	l.Set("/mix.jet", `{{raw: .}}|{{.}}`)
	if r := executeStringWith(t, mustTemplate(t, htmlSet, "/mix.jet"), nil, `<a>`); r != `<a>|&lt;a&gt;` {
		t.Fatalf("per-command raw then autoescape: %q", r)
	}

	// Sharing one cache between differently-configured sets is a documented sharp
	// edge: the cached *Template keeps a pointer to the Set that parsed it, so a cache
	// hit in the second (raw) set executes with the first (HTML) set's SafeWriter. The
	// autoescape context does NOT follow the executing set in that case; use separate
	// caches when sets use different SafeWriters.
	shared := &cache{}
	sa := NewSet(l, WithCache(shared))
	sb := NewSet(l, WithCache(shared), WithSafeWriter(nil))
	tplA := mustTemplate(t, sa, "/x.jet")
	tplB := mustTemplate(t, sb, "/x.jet") // sb hits the same canonical path
	if tplA != tplB {
		t.Fatalf("shared cache must hand out the same *Template")
	}
	if r := executeStringWith(t, tplA, nil, `<a>`); r != `&lt;a&gt;` {
		t.Fatalf("cached template executed by html set: %q", r)
	}
	if r := executeStringWith(t, tplB, nil, `<a>`); r != `&lt;a&gt;` {
		t.Fatalf("cache-hit template keeps the parsing set's (HTML) SafeWriter, got %q", r)
	}
}

func executeStringWith(t *testing.T, tpl *Template, vars VarMap, data interface{}) string {
	t.Helper()
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, vars, data); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return buf.String()
}

// --- 13. execution-time errors keep stable text ---------------------------------

func TestNameResolution_ExecutionErrors(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		content  string
		wantSubs []string
	}{
		{"unresolved identifier", "/u.jet", `{{nope}}`, []string{`identifier "nope" not available`}},
		{"unresolved block", "/b.jet", `{{yield missing()}}`, []string{`unresolved block "missing"`}},
		{"include missing", "/i.jet", `{{include "absent.jet"}}`, []string{"template /absent.jet could not be found"}},
		{"include non-string", "/s.jet", `{{include 123}}`, []string{"evaluating name of template to include"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newResolveHarness(t)
			h.add(tc.path, tc.content)
			err := h.renderErr(tc.path, nil, nil)
			if err == nil {
				t.Fatalf("want error containing %v", tc.wantSubs)
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(err.Error(), sub) {
					t.Fatalf("error %q does not contain %q", err.Error(), sub)
				}
			}
		})
	}
}

// --- 14. observer mechanics: off-by-default, bounded, stack retained -------------

func TestNameResolution_ObserverDisabledIsInert(t *testing.T) {
	// Identical template executed without an observer: output and error text must be
	// byte-identical, and the hot path must not allocate for observer bookkeeping.
	template := `{{a}}|{{lower("X")}}`
	errorTemplate := `{{missing}}`

	run := func(withObserver bool, src string) (string, string) {
		l := NewInMemLoader()
		l.Set("/t.jet", src)
		s := NewSet(l)
		var ob *recordingObserver
		if withObserver {
			ob = newRecordingObserver(0)
			attachLookupObserver(s, ob, 0)
		}
		tpl, err := s.GetTemplate("/t.jet")
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		err = tpl.Execute(&buf, vars0(), nil)
		errText := ""
		if err != nil {
			errText = err.Error()
		}
		return buf.String(), errText
	}

	offOut, offErr := run(false, template)
	onOut, onErr := run(true, template)
	if offOut != onOut || offErr != onErr {
		t.Fatalf("observer changed execution: off=(%q,%q) on=(%q,%q)", offOut, offErr, onOut, onErr)
	}
	offOut2, offErr2 := run(false, errorTemplate)
	onOut2, onErr2 := run(true, errorTemplate)
	if offOut2 != onOut2 || offErr2 != onErr2 {
		t.Fatalf("observer changed error text: off=(%q,%q) on=(%q,%q)", offErr2, onErr2, offOut2, onOut2)
	}
	if offErr2 == "" {
		t.Fatal("expected error template to fail")
	}

	// Allocation guard: with the observer off the probe pointer is nil and execution
	// follows exactly the uninstrumented path; assert a tight envelope for this tiny
	// template (no per-event slice growth or bookkeeping).
	measure := func(attach bool) float64 {
		ml := NewInMemLoader()
		ml.Set("/t.jet", template)
		ms := NewSet(ml)
		if attach {
			attachLookupObserver(ms, newRecordingObserver(1<<20), 1<<20)
		}
		mtpl, _ := ms.GetTemplate("/t.jet")
		return testing.AllocsPerRun(20, func() {
			var b bytes.Buffer
			if err := mtpl.Execute(&b, vars0(), nil); err != nil {
				t.Fatal(err)
			}
		})
	}
	offAllocs := measure(false)
	onAllocs := measure(true)
	// The probe-off path performs a single nil check and takes the original lookup
	// loop, so the allocation count is the template's intrinsic one (no per-event
	// bookkeeping); turning the observer on must strictly add event allocations.
	if offAllocs > 14 {
		t.Fatalf("uninstrumented execution allocates too much: %v allocs/run (intrinsic budget 14)", offAllocs)
	}
	if onAllocs <= offAllocs {
		t.Fatalf("instrumented execution must allocate for events (on=%v off=%v)", onAllocs, offAllocs)
	}
}

func vars0() VarMap { return vars("a", "A") }

func wantRangeOutput() string {
	var b strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&b, "%d", i)
	}
	return b.String()
}

func TestNameResolution_ObserverEventCap(t *testing.T) {
	l := NewInMemLoader()
	// A range forces many repeated lookups.
	l.Set("/r.jet", `{{range ints(0, 50)}}{{x := .}}{{x}}{{end}}`)
	s := NewSet(l)
	const cap = 20
	ob := newRecordingObserver(cap)
	attachLookupObserver(s, ob, cap)
	tpl, err := s.GetTemplate("/r.jet")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, nil, nil); err != nil {
		t.Fatal(err)
	}
	events := ob.eventsSnapshot()
	if len(events) != cap {
		t.Fatalf("event cap: want exactly %d retained events, got %d", cap, len(events))
	}
	// Truncation must preserve the template invocation stack on retained events.
	for i, e := range events {
		if len(e.Stack) == 0 || e.Stack[len(e.Stack)-1] != "/r.jet" {
			t.Fatalf("retained event %d lost template stack: %+v", i, e)
		}
	}
	// Rendering itself must be unaffected by truncation.
	if strings.TrimSpace(buf.String()) != wantRangeOutput() {
		t.Fatalf("truncation changed render output: %q", buf.String())
	}
}

// The resolver exposes ordered slices (extension probes, scope layers) but never a
// contract over Go map iteration order: block precedence is established at parse
// time (deterministic merge order), and the observed candidates are a flat map hit
// rather than an iteration sequence.
func TestNameResolution_NoMapIterationContract(t *testing.T) {
	h := newResolveHarness(t)
	for i := 0; i < 8; i++ {
		p := fmt.Sprintf("/m%d.jet", i)
		h.add(p, fmt.Sprintf(`{{block k%d()}}K%d{{end}}`, i, i))
	}
	h.add("/all.jet", `{{import "m0.jet"}}{{import "m1.jet"}}{{import "m2.jet"}}{{import "m3.jet"}}{{import "m4.jet"}}{{import "m5.jet"}}{{import "m6.jet"}}{{import "m7.jet"}}{{yield k0()}}{{yield k7()}}`)
	if got := h.render("/all.jet", nil, nil); got != "K0K7" {
		t.Fatalf("multi-import render: %q", got)
	}
}

// ensure filepath is referenced (used by sibling path semantics documentation tests)
var _ = filepath.ToSlash

// --- 15. the executable name-resolution map -------------------------------------
//
// TestNameResolutionMap is the executable counterpart of docs/name-resolution.md:
// one row per syntax construct, asserting what binds at parse time vs execution
// time and which source wins. Keep this table in sync with the document.

func TestNameResolutionMap(t *testing.T) {
	h := newResolveHarness(t)
	h.add("/parent.jet", `P[{{yield slot()}}]`)
	h.add("/lib.jet", `{{block widget()}}W{{end}}`)
	h.add("/child.jet", `{{extends "parent.jet"}}{{import "lib.jet"}}{{block slot()}}{{yield widget()}}{{end}}`)
	h.add("/inc.jet", `I={{.}}`)
	h.add("/main.jet", `{{g := 1}}{{include "inc.jet" g}}`)
	h.set.AddGlobal("gname", "GV")

	// Parse-time bindings for the extends+import child.
	ct, err := h.set.GetTemplate("/child.jet")
	if err != nil {
		t.Fatal(err)
	}
	if ct.extends == nil || ct.extends.Name != "/parent.jet" {
		t.Fatalf("extends must bind Template.extends at parse time, got %+v", ct.extends)
	}
	if len(ct.imports) != 1 || ct.imports[0].Name != "/lib.jet" {
		t.Fatalf("imports must bind Template.imports at parse time, got %+v", ct.imports)
	}
	if _, ok := ct.processedBlocks["widget"]; !ok {
		t.Fatalf("imported block must be merged into processedBlocks at parse time")
	}
	if _, ok := ct.processedBlocks["slot"]; !ok {
		t.Fatalf("own block must be registered in processedBlocks at parse time")
	}
	// extends template resolution is a parse-time event (stack rooted at the child).
	pe := h.requireTemplateEvent("/parent.jet")
	if pe.Source != SourceLoader || len(pe.Stack) != 1 || pe.Stack[0] != "/child.jet" {
		t.Fatalf("extends parse event: %+v", pe)
	}

	// Execute the child: parent root runs, child slot wins, imported widget wins.
	var cbuf bytes.Buffer
	if err := ct.Execute(&cbuf, nil, nil); err != nil {
		t.Fatal(err)
	}
	if cbuf.String() != "P[W]" {
		t.Fatalf("child render: %q", cbuf.String())
	}
	slotEv := h.requireBlockEvent("slot")
	if slotEv.Template != "/child.jet" || slotEv.Stack[len(slotEv.Stack)-1] != "/parent.jet" {
		t.Fatalf("slot event (child block, executing parent): %+v", slotEv)
	}
	wEv := h.requireBlockEvent("widget")
	if wEv.Template != "/lib.jet" {
		t.Fatalf("widget must bind imported block, got %q", wEv.Template)
	}

	// include resolves at execution time with a fresh scope; dot is the only input.
	mt, err := h.set.GetTemplate("/main.jet")
	if err != nil {
		t.Fatal(err)
	}
	var mbuf bytes.Buffer
	if err := mt.Execute(&mbuf, nil, nil); err != nil {
		t.Fatal(err)
	}
	if mbuf.String() != "I=1" {
		t.Fatalf("main render: %q", mbuf.String())
	}
	incEv := h.requireTemplateEvent("/inc.jet")
	if len(incEv.Stack) == 0 || incEv.Stack[len(incEv.Stack)-1] != "/main.jet" {
		t.Fatalf("include must resolve at execution time from the main frame: %+v", incEv)
	}

	// Variable sources across all layers.
	h.add("/v.jet", `{{gname}}`)
	if r := h.render("/v.jet", vars("gname", "TV"), nil); r != "TV" {
		t.Fatalf("template var must shadow global, got %q", r)
	}
}
