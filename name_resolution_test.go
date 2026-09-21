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
	"bytes"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This suite is an executable specification of Jet name resolution. Every test
// asserts both the rendered output and the decisive resolution path captured by
// the internal LookupRecorder. It uses only the in-memory loader; nothing here
// touches the network, wall clock or filesystem directory iteration order.

// renderResult bundles what one observed render produces.
type renderResult struct {
	out   string
	err   error
	rec   *LookupRecorder
	parse []parseBindEvent
}

// renderObserved parses entry from a fresh Set built on loader, attaches rec as
// both the parse-time and run-time observer, executes with vars/data and returns
// everything the assertions need. The fresh Set guarantees per-test cache and
// global isolation.
func renderObserved(t *testing.T, loader Loader, entry string, vars VarMap, data interface{}, opts ...Option) renderResult {
	t.Helper()
	rec := NewLookupRecorder()
	allOpts := append([]Option{func(s *Set) { s.parseObserver = rec }}, opts...)
	set := NewSet(loader, allOpts...)
	tpl, err := set.GetTemplate(entry)
	if err != nil {
		t.Fatalf("parse %s: %v", entry, err)
	}
	var buf bytes.Buffer
	execErr := tpl.execute(&buf, vars, data, rec)
	return renderResult{
		out:   buf.String(),
		err:   execErr,
		rec:   rec,
		parse: rec.ParseEvents(),
	}
}

// mustRender fails the test if rendering returned an error, printing the full
// decisive path of every recorded lookup for context.
func (r renderResult) mustRender(t *testing.T) renderResult {
	t.Helper()
	if r.err != nil {
		var b strings.Builder
		b.WriteString(r.err.Error())
		for _, ev := range r.rec.Events() {
			b.WriteString("\n\t" + ev.Describe())
		}
		t.Fatalf("unexpected render error: %s", b.String())
	}
	return r
}

func (r renderResult) expectOut(t *testing.T, want string) renderResult {
	t.Helper()
	if r.out != want {
		t.Fatalf("render output mismatch:\n\twant %q\n\tgot  %q", want, r.out)
	}
	return r
}

func (r renderResult) expectErr(t *testing.T, wantSubstring string) renderResult {
	t.Helper()
	if r.err == nil {
		t.Fatalf("expected error containing %q, got nil; output=%q", wantSubstring, r.out)
	}
	if !strings.Contains(r.err.Error(), wantSubstring) {
		t.Fatalf("expected error containing %q, got %q", wantSubstring, r.err.Error())
	}
	return r
}

// lookup asserts that a variable/block lookup for name happened and returns its
// decisive event. If several lookups share the name the first is returned,
// matching the "one lookup" observation model.
func (r renderResult) lookup(t *testing.T, kind lookupKind, name string) lookupEvent {
	t.Helper()
	ev := r.rec.findEvent(kind, name)
	if ev == nil {
		var got []string
		for _, e := range r.rec.Events() {
			got = append(got, string(e.Kind)+":"+e.Name)
		}
		t.Fatalf("no %s lookup for %q was recorded; recorded: %v", kind, name, got)
	}
	return *ev
}

func expectSelected(t *testing.T, ev lookupEvent, want lookupSource) {
	t.Helper()
	if ev.Selected != want {
		t.Fatalf("lookup %q: expected selected source %s, got %s\npath: %s", ev.Name, want, ev.Selected, ev.Describe())
	}
}

func expectStack(t *testing.T, ev lookupEvent, want ...string) {
	t.Helper()
	got := make([]string, len(ev.Stack))
	for i, f := range ev.Stack {
		got[i] = f.String()
	}
	if len(got) != len(want) {
		t.Fatalf("lookup %q: expected stack %v, got %v\npath: %s", ev.Name, want, got, ev.Describe())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lookup %q: expected stack %v, got %v\npath: %s", ev.Name, want, got, ev.Describe())
		}
	}
}

// expectCandidates asserts the ordered list of probed layers and their presence.
func expectCandidates(t *testing.T, ev lookupEvent, want ...lookupCandidate) {
	t.Helper()
	got := ev.Candidates
	if len(got) != len(want) {
		t.Fatalf("lookup %q: expected %d candidates %+v, got %d %+v\npath: %s", ev.Name, len(want), want, len(got), got, ev.Describe())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lookup %q: candidate %d expected %+v, got %+v\npath: %s", ev.Name, i, want[i], got[i], ev.Describe())
		}
	}
}

// parseBindingsFor filters parse-time events for one block name.
func (r renderResult) parseBindingsFor(t *testing.T, blockName string) []parseBindEvent {
	t.Helper()
	var out []parseBindEvent
	for _, e := range r.parse {
		if e.Name == blockName {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no parse bindings recorded for block %q; recorded: %+v", blockName, r.parse)
	}
	return out
}

// parseBindingsOn returns bindings into the processedBlocks table of template.
func (r renderResult) parseBindingsOn(templatePath string) []parseBindEvent {
	var out []parseBindEvent
	for _, e := range r.parse {
		if e.Template == filepath.ToSlash(templatePath) {
			out = append(out, e)
		}
	}
	return out
}

// sortedOrigins returns the distinct origins among bindings in deterministic
// order so tests never depend on map iteration order.
func sortedOrigins(events []parseBindEvent) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range events {
		if !seen[e.Origin] {
			seen[e.Origin] = true
			out = append(out, e.Origin)
		}
	}
	sort.Strings(out)
	return out
}

func rv(v interface{}) reflect.Value { return reflect.ValueOf(v) }
