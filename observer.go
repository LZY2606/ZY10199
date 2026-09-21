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

// This file contains internal, opt-in instrumentation used to characterize Jet's
// name resolution. It is deliberately unexported: no public API is added and
// when no observer is attached the lookup paths are bit-for-bit identical to the
// uninstrumented engine (the hooks are guarded by a nil check at every site).

import (
	"fmt"
	"strings"
)

// lookupKind distinguishes the two namespaces a name can resolve in at run time.
type lookupKind string

const (
	// lookupVariable is an identifier-to-value lookup performed by Runtime.resolve
	// (lexical scope chain, then set globals, then built-in default variables).
	lookupVariable lookupKind = "variable"
	// lookupBlock is a named block ("macro") lookup performed by scope.getBlock
	// (the block tables attached to the lexical scope chain).
	lookupBlock lookupKind = "block"
)

// lookupSource names the layer a value/block was selected from.
type lookupSource string

const (
	sourceUnresolved   lookupSource = "unresolved"
	sourceContext      lookupSource = "context"       // the special "." identifier
	sourceLocalScope   lookupSource = "local-scope"   // current (innermost) runtime scope
	sourceParentScope  lookupSource = "parent-scope"  // any non-top lexical scope on the chain
	sourceExecuteScope lookupSource = "execute-scope" // the scope seeded from Template.Execute's variables
	sourceGlobal       lookupSource = "set-global"    // a variable registered with Set.AddGlobal
	sourceDefault      lookupSource = "default"       // a built-in default variable/function
	sourceBlockTable   lookupSource = "block-table"   // a block table found on the scope chain
)

// lookupCandidate describes one layer probed while resolving a name, in the
// order it was probed. Candidates are recorded even when the layer does not
// contain the name (Present == false) so tests can assert the full walk.
type lookupCandidate struct {
	// Layer is the layer that was probed: "context", a specific scope depth
	// ("scope:N"), "set-global", "default", or a block table at "blocks:N".
	Layer string
	// Present reports whether the name existed in that layer.
	Present bool
}

// templateFrame is one entry of the template call stack captured with an event.
type templateFrame struct {
	// Kind is how the frame was entered: "execute" (Template.Execute),
	// "include" ({{ include ... }}), "includeIfExists" or "exec" builtins.
	Kind string
	// Template is the slash-delimited path of the template whose root is run.
	Template string
}

func (f templateFrame) String() string {
	return f.Kind + ":" + f.Template
}

// lookupEvent is one fully resolved (or failed) name lookup.
type lookupEvent struct {
	Kind       lookupKind
	Name       string
	StartAt    int // depth of the scope the walk started at (0 = execute scope)
	Candidates []lookupCandidate
	Selected   lookupSource
	// SelectedDepth is the scope/block-table depth the name was selected at.
	// It is -1 when the name came from context/globals/defaults or was not found.
	SelectedDepth int
	// SelectedTemplate records the defining template of a selected block.
	SelectedTemplate string
	Found            bool
	Stack            []templateFrame
	// snapshot, when non-nil, lazily captures the template call stack only for
	// events that are actually retained, so dropped events cost no stack copy.
	liveStack *[]templateFrame
}

// parseBindEvent records a single parse-time binding decision: the merge of a
// block into a template's processedBlocks table. Shadowed is true when the
// target table already held a block under the same name.
type parseBindEvent struct {
	Template string // template whose processedBlocks table was updated
	Name     string
	Origin   string // template the block is physically defined in
	Phase    string // "extends", "import", or "self"
	Shadowed bool
}

// lookupObserver is implemented by anything that wants to consume name
// resolution events. The concrete recorder below is the production stand-in;
// tests may implement their own.
type lookupObserver interface {
	variableEvent(lookupEvent)
	blockEvent(lookupEvent)
	parseEvent(parseBindEvent)
}

// defaultLookupEventCap bounds the number of events a LookupRecorder keeps so a
// runaway render (e.g. a long range) cannot grow memory without limit. It only
// caps retained history; it never changes what the engine executes.
const defaultLookupEventCap = 4096

// LookupRecorder is the opt-in collector handed to a Runtime. It is safe for a
// single render at a time (a Runtime is not shared between goroutines).
type LookupRecorder struct {
	events      []lookupEvent
	parseEvents []parseBindEvent
	// EventCap overrides defaultLookupEventCap when > 0.
	EventCap int
	// truncated reports that at least one lookup event was dropped.
	truncated bool
	// LastStack preserves the template stack from the most recent lookup even
	// after truncation, so a failure under an event cap stays diagnosable.
	lastStack []templateFrame
	stack     []templateFrame
}

// NewLookupRecorder returns an empty recorder with the default event cap.
func NewLookupRecorder() *LookupRecorder {
	return &LookupRecorder{}
}

func (r *LookupRecorder) cap_() int {
	if r.EventCap > 0 {
		return r.EventCap
	}
	return defaultLookupEventCap
}

func (r *LookupRecorder) record(ev lookupEvent) {
	if len(r.events) < r.cap_() {
		if ev.liveStack != nil {
			ev.Stack = r.copyStack(*ev.liveStack)
		}
		r.events = append(r.events, ev)
		// The newest retained event carries the most recent stack snapshot; no
		// copying is done for events dropped due to the cap, keeping the cap
		// cheap even during a deep include chain.
		r.lastStack = ev.Stack
	}
	if len(r.events) >= r.cap_() {
		r.truncated = true
	}
}

func (r *LookupRecorder) copyStack(src []templateFrame) []templateFrame {
	if len(src) == 0 {
		return nil
	}
	out := make([]templateFrame, len(src))
	copy(out, src)
	return out
}

func (r *LookupRecorder) variableEvent(ev lookupEvent) { r.record(ev) }
func (r *LookupRecorder) blockEvent(ev lookupEvent)    { r.record(ev) }

func (r *LookupRecorder) parseEvent(ev parseBindEvent) {
	r.parseEvents = append(r.parseEvents, ev)
}

// Events returns the recorded lookup events (a copy of the slice header; the
// underlying records must not be mutated by callers).
func (r *LookupRecorder) Events() []lookupEvent { return r.events }

// ParseEvents returns recorded parse-time block bindings.
func (r *LookupRecorder) ParseEvents() []parseBindEvent { return r.parseEvents }

// Truncated reports whether lookup events were dropped due to the cap.
func (r *LookupRecorder) Truncated() bool { return r.truncated }

// LastStack returns the template stack of the most recent lookup.
func (r *LookupRecorder) LastStack() []templateFrame { return r.lastStack }

// Reset empties the recorder and its template stack.
func (r *LookupRecorder) Reset() {
	r.events = r.events[:0]
	r.parseEvents = r.parseEvents[:0]
	r.stack = r.stack[:0]
	r.lastStack = r.lastStack[:0]
	r.truncated = false
}

// pushFrame/popFrame maintain the logical template call stack.
func (r *LookupRecorder) pushFrame(kind, template string) {
	r.stack = append(r.stack, templateFrame{Kind: kind, Template: template})
}

func (r *LookupRecorder) popFrame() {
	if len(r.stack) > 0 {
		r.stack = r.stack[:len(r.stack)-1]
	}
}

// Describe renders the decisive path of a lookup event on one human-readable
// line; tests use it for failure messages.
func (ev lookupEvent) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %q start=scope:%d candidates=[", ev.Kind, ev.Name, ev.StartAt)
	for i, c := range ev.Candidates {
		if i > 0 {
			b.WriteByte(' ')
		}
		hit := '-'
		if c.Present {
			hit = '+'
		}
		fmt.Fprintf(&b, "%s%c", c.Layer, hit)
	}
	fmt.Fprintf(&b, "] selected=%s", ev.Selected)
	if ev.SelectedDepth >= 0 {
		fmt.Fprintf(&b, "@%d", ev.SelectedDepth)
	}
	if ev.SelectedTemplate != "" {
		fmt.Fprintf(&b, " in %s", ev.SelectedTemplate)
	}
	if len(ev.Stack) > 0 {
		b.WriteString(" stack=")
		for i, f := range ev.Stack {
			if i > 0 {
				b.WriteByte('>')
			}
			b.WriteString(f.String())
		}
	}
	return b.String()
}

// findEvent returns the first recorded lookup for name and kind, or nil.
func (r *LookupRecorder) findEvent(kind lookupKind, name string) *lookupEvent {
	for i := range r.events {
		if r.events[i].Kind == kind && r.events[i].Name == name {
			return &r.events[i]
		}
	}
	return nil
}
