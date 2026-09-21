// Copyright 2016 José Santos <henrique_1609@me.com>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package jet

import (
	"reflect"
	"sync"
)

// Lookup kinds observed by the internal name-resolution probe.
const (
	// LookupVariable is a variable/identifier resolution performed by (*Runtime).resolve
	// (op is "read"), an assignment through (*Runtime).setValue (op is "assign"), or a
	// short variable declaration placed in the current scope (op is "let").
	LookupVariable = "variable"
	// LookupBlock is a block/macro resolution performed by (*scope).getBlock
	// (used by both yield and block nodes at execution time).
	LookupBlock = "block"
	// LookupTemplate is a template resolution performed through (*Set).getTemplate,
	// covering cache lookups, loader probes (Exists with each configured extension),
	// parsing and the eventual cache/loader hit.
	LookupTemplate = "template"
)

// Sources from which a resolved name can originate. They describe the layer that
// "won" a lookup; see docs/name-resolution.md for the full precedence graph.
const (
	// SourceLocal is the innermost execution scope created by a := declaration,
	// a block/yield parameter binding, an if/range/try clause or an include.
	SourceLocal = "local"
	// SourceTemplate is a variable passed to Template.Execute or an outer execution
	// scope of the same template invocation chain.
	SourceTemplate = "template"
	// SourceGlobal is a Set global registered through Set.AddGlobal/AddGlobalFunc.
	SourceGlobal = "global"
	// SourceDefault is one of Jet's built-in default variables (e.g. lower, len, html).
	SourceDefault = "default"
	// SourceBlock is a block/macro node; the Template field names the template that
	// declared it (the child itself, an extended parent or an imported library).
	SourceBlock = "block"
	// SourceCache means the template was served from the Set's cache.
	SourceCache = "cache"
	// SourceLoader means the template was loaded and parsed through the Loader.
	SourceLoader = "loader"
)

// ScopeCandidate records one layer inspected while resolving a name.
type ScopeCandidate struct {
	// Kind is "local" for a scope created during execution and "template" for the
	// scope holding the variables passed to Template.Execute.
	Kind string
	// Depth is 0 for the scope the lookup started in, increasing towards the root.
	Depth int
	// Hit reports whether this layer contained the looked-up name.
	Hit bool
	// BlockCount is the number of blocks visible from this scope (for block lookups).
	BlockCount int
}

// TemplateCandidate records one cache or loader probe performed while resolving a
// template path. Probes are emitted in the order the Set tries them (which is the
// configured extension order, an ordered slice - never map iteration order).
type TemplateCandidate struct {
	// Stage is "cache", "loader-exists", "loader-open" or "parse".
	Stage string
	// Path is the exact canonical path probed (including the tried extension).
	Path string
	// Hit reports whether this probe supplied the template.
	Hit bool
}

// LookupEvent is a single name-resolution observation.
type LookupEvent struct {
	// Kind is one of LookupVariable, LookupBlock or LookupTemplate.
	Kind string
	// Op further qualifies variable lookups: "read", "assign" or "let".
	Op string
	// Name is the identifier, block name or logical template path being resolved.
	Name string
	// Candidates are the scope layers (variables/blocks) inspected, inner to outer.
	Candidates []ScopeCandidate
	// TemplateCandidates are the cache/loader probes performed, in attempt order.
	TemplateCandidates []TemplateCandidate
	// Source is the winning layer (see the Source* constants). Empty for misses.
	Source string
	// Template is the template that owns the winning block (block lookups only).
	Template string
	// Found reports whether the lookup succeeded.
	Found bool
	// Stack is the template invocation stack at the moment of the lookup, with the
	// currently executing template last. It is always populated, even when the event
	// buffer has to truncate earlier events.
	Stack []string
}

// LookupObserver receives name-resolution events. Implementations must be safe for
// concurrent use if a Set is executed concurrently.
type LookupObserver interface {
	ObserveLookup(LookupEvent)
}

// DefaultObserverEventLimit is the default per-observer event cap.
const DefaultObserverEventLimit = 4096

// lookupProbe is the internal, allocation-aware view attached to a Set. Runtime
// instances pick it up when they start executing a template.
type lookupProbe struct {
	observer LookupObserver
	limit    int
}

var probeRegistry sync.Map // map[*Set]*lookupProbe

// attachLookupObserver installs ob on s. It is an internal test scaffold; it is not
// part of Jet's public API. At most limit events are retained (DefaultObserverEventLimit
// when limit <= 0); once the cap is reached further events are dropped, with every
// retained event still carrying the template invocation stack.
func attachLookupObserver(s *Set, ob LookupObserver, limit int) {
	if limit <= 0 {
		limit = DefaultObserverEventLimit
	}
	probeRegistry.Store(s, &lookupProbe{observer: ob, limit: limit})
}

func detachLookupObserver(s *Set) {
	probeRegistry.Delete(s)
}

func lookupProbeFor(s *Set) *lookupProbe {
	if p, ok := probeRegistry.Load(s); ok {
		return p.(*lookupProbe)
	}
	return nil
}

// probe is the per-execution probe state. Every event-producing call site performs a
// single nil check on *Runtime.probe, so executing with no observer attached allocates
// nothing and changes neither scheduling nor error text.
type probe struct {
	target *lookupProbe
	count  int
	stack  []string
}

func (st *Runtime) emit(event LookupEvent) {
	if st.probe == nil {
		return
	}
	if st.probe.count >= st.probe.target.limit {
		return
	}
	event.Stack = append(event.Stack, st.probe.stack...)
	st.probe.target.observer.ObserveLookup(event)
	st.probe.count++
}

// pushTemplateFrame/popTemplateFrame maintain the ordered template invocation stack.
func (st *Runtime) pushTemplateFrame(name string) {
	if st.probe == nil {
		return
	}
	st.probe.stack = append(st.probe.stack, name)
}

func (st *Runtime) popTemplateFrame() {
	if st.probe == nil {
		return
	}
	st.probe.stack = st.probe.stack[:len(st.probe.stack)-1]
}

// probeStack returns a snapshot copy of the current template invocation stack.
func (st *Runtime) probeStack() []string {
	if st.probe == nil {
		return nil
	}
	return append([]string(nil), st.probe.stack...)
}

// recordingObserver is a simple bounded LookupObserver used by the internal tests.
type recordingObserver struct {
	mu     sync.Mutex
	events []LookupEvent
	limit  int
}

func newRecordingObserver(limit int) *recordingObserver {
	if limit <= 0 {
		limit = DefaultObserverEventLimit
	}
	return &recordingObserver{limit: limit}
}

func (o *recordingObserver) ObserveLookup(e LookupEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.events) >= o.limit {
		return
	}
	o.events = append(o.events, e)
}

func (o *recordingObserver) eventsSnapshot() []LookupEvent {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]LookupEvent, len(o.events))
	copy(out, o.events)
	return out
}

// variable candidates helpers -------------------------------------------------

// variableCandidates enumerates every name layer considered for a variable lookup
// (each execution scope inner-to-outer, then Set globals, then built-in defaults),
// marks the first layer that contains the name as the winner and returns it. It is
// only called when a probe is attached, so the extra enumeration costs nothing on
// the uninstrumented execution path.
func (st *Runtime) variableCandidates(name string) ([]ScopeCandidate, string, reflect.Value) {
	candidates := make([]ScopeCandidate, 0, 4)
	winner := ""
	var winnerValue reflect.Value
	depth := 0
	note := func(kind string, hit bool, v reflect.Value) {
		c := ScopeCandidate{Kind: kind, Depth: depth, Hit: hit}
		if st.scope != nil {
			c.BlockCount = len(st.scope.blocks)
		}
		candidates = append(candidates, c)
		if hit && winner == "" {
			winner = kind
			winnerValue = v
		}
		depth++
	}
	for sc := st.scope; sc != nil; sc = sc.parent {
		// The initial execution scope (st.rootScope) holds the variables passed to
		// Template.Execute; every scope created with newScope() during execution is local.
		kind := SourceLocal
		if sc == st.rootScope {
			kind = SourceTemplate
		}
		v, hit := sc.variables[name]
		note(kind, hit, v)
	}

	st.set.gmx.RLock()
	v, hit := st.set.globals[name]
	st.set.gmx.RUnlock()
	note(SourceGlobal, hit, v)

	v, hit = defaultVariables[name]
	note(SourceDefault, hit, v)

	return candidates, winner, winnerValue
}
