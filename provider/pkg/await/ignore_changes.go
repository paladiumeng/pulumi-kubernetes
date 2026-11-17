// Copyright 2016-2026, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package await

import (
	"sort"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
	"sigs.k8s.io/structured-merge-diff/v6/value"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
)

// ignoreTarget is one concrete location that an ignoreChanges path names.
//
// An ignoreChanges path addresses a list item by its index, such as
// `spec.template.spec.containers[0].image`. The managed fields of an object
// address the same item by its key, such as `containers[name="web"]`, and the
// live object can hold the item at another index. So one target keeps the
// location in each of the three forms.
type ignoreTarget struct {
	// managed is the location as the managed fields record it.
	managed fieldpath.Path
	// inputs is the location in the new inputs. A string is a map key and an
	// int is a list index.
	inputs []any
	// live is the location in the live object, in the same form as inputs. It
	// is only valid when liveFound is true.
	live []any
	// liveFound is true when the live object has a value at the location.
	liveFound bool
}

// ignoreCursor is a value reached while walking an object, or the absence of
// one.
type ignoreCursor struct {
	val any
	ok  bool
}

// resolveIgnoreTargets expands an ignoreChanges path into the locations it
// names. A wildcard element yields one target for each item of the list or
// map it stands for. managed holds the fields of every field manager, with
// their parent paths.
func resolveIgnoreTargets(
	path resource.PropertyPath, inputs, live map[string]any, managed *fieldpath.Set,
) []ignoreTarget {
	r := &ignoreResolver{managed: managed}
	r.walk(path, ignoreCursor{inputs, true}, ignoreCursor{live, true}, ignoreTarget{})
	return r.targets
}

type ignoreResolver struct {
	managed *fieldpath.Set
	targets []ignoreTarget
}

func (r *ignoreResolver) walk(rest resource.PropertyPath, in, lv ignoreCursor, t ignoreTarget) {
	if len(rest) == 0 {
		t.liveFound = lv.ok
		r.targets = append(r.targets, t)
		return
	}

	switch e := rest[0].(type) {
	case string:
		if e == "*" {
			r.walkWildcard(rest[1:], in, lv, t)
			return
		}
		name := e
		t.managed = appendPathElement(t.managed, fieldpath.PathElement{FieldName: &name})
		t.inputs = appendLocation(t.inputs, name)
		t.live = appendLocation(t.live, name)
		r.walk(rest[1:], mapItem(in, name), mapItem(lv, name), t)
	case int:
		inItem := listItem(in, e)
		pe, liveIndex := r.listElement(t.managed, e, inItem, lv)
		t.managed = appendPathElement(t.managed, pe)
		t.inputs = appendLocation(t.inputs, e)
		t.live = appendLocation(t.live, liveIndex)
		r.walk(rest[1:], inItem, listItem(lv, liveIndex), t)
	}
}

// walkWildcard continues the walk for each item of the list or map at the
// cursor. It reads the items from the inputs, and from the live object when
// the inputs have no value there.
func (r *ignoreResolver) walkWildcard(rest resource.PropertyPath, in, lv ignoreCursor, t ignoreTarget) {
	source := in
	if !source.ok {
		source = lv
	}
	switch items := source.val.(type) {
	case []any:
		for i := range items {
			r.walk(append(resource.PropertyPath{i}, rest...), in, lv, t)
		}
	case map[string]any:
		keys := make([]string, 0, len(items))
		for k := range items {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			r.walk(append(resource.PropertyPath{k}, rest...), in, lv, t)
		}
	}
}

// listElement returns how the managed fields address item `index` of the list
// at prefix, and the index of that item in the live list. The live index is
// -1 when the live list has no such item.
//
// The managed fields address an item of an associative list by its key
// fields, and an item of a set by its value. listElement finds the recorded
// element that matches the item of the inputs. If none matches, for example
// because the inputs leave out a key field that has a default, it tries the
// live item at the same index. A list with no recorded elements, such as an
// atomic list, is addressed by index.
func (r *ignoreResolver) listElement(
	prefix fieldpath.Path, index int, inItem, lvList ignoreCursor,
) (fieldpath.PathElement, int) {
	liveItems, _ := lvList.val.([]any)

	var refs []any
	if inItem.ok {
		refs = append(refs, inItem.val)
	}
	if index < len(liveItems) {
		refs = append(refs, liveItems[index])
	}

	candidates := childElements(r.managed, prefix)
	for _, ref := range refs {
		for _, candidate := range candidates {
			if !elementMatches(candidate, ref) {
				continue
			}
			for i, item := range liveItems {
				if elementMatches(candidate, item) {
					return candidate, i
				}
			}
			return candidate, -1
		}
	}

	liveIndex := -1
	if index < len(liveItems) {
		liveIndex = index
	}
	return fieldpath.PathElement{Index: &index}, liveIndex
}

// childElements returns the list elements that the set records directly under
// prefix.
func childElements(s *fieldpath.Set, prefix fieldpath.Path) []fieldpath.PathElement {
	var out []fieldpath.PathElement
	seen := map[string]struct{}{}
	s.Iterate(func(p fieldpath.Path) {
		if len(p) <= len(prefix) || !p[:len(prefix)].Equals(prefix) {
			return
		}
		pe := p[len(prefix)]
		if pe.Key == nil && pe.Value == nil {
			return
		}
		id := pe.String()
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, pe)
	})
	return out
}

// elementMatches reports whether a list element of the managed fields
// addresses the given list item.
func elementMatches(pe fieldpath.PathElement, item any) bool {
	switch {
	case pe.Key != nil:
		m, ok := item.(map[string]any)
		if !ok {
			return false
		}
		for _, field := range *pe.Key {
			v, ok := m[field.Name]
			if !ok || !value.Equals(field.Value, value.NewValueInterface(v)) {
				return false
			}
		}
		return true
	case pe.Value != nil:
		return value.Equals(*pe.Value, value.NewValueInterface(item))
	default:
		return false
	}
}

func mapItem(c ignoreCursor, key string) ignoreCursor {
	m, ok := c.val.(map[string]any)
	if !c.ok || !ok {
		return ignoreCursor{}
	}
	v, ok := m[key]
	return ignoreCursor{v, ok}
}

func listItem(c ignoreCursor, index int) ignoreCursor {
	l, ok := c.val.([]any)
	if !c.ok || !ok || index < 0 || index >= len(l) {
		return ignoreCursor{}
	}
	return ignoreCursor{l[index], true}
}

// appendPathElement and appendLocation copy the slice, so the targets of a
// wildcard do not share a backing array.
func appendPathElement(p fieldpath.Path, pe fieldpath.PathElement) fieldpath.Path {
	out := make(fieldpath.Path, len(p), len(p)+1)
	copy(out, p)
	return append(out, pe)
}

func appendLocation(loc []any, e any) []any {
	out := make([]any, len(loc), len(loc)+1)
	copy(out, loc)
	return append(out, e)
}

// nestedValue returns a deep copy of the value at loc.
func nestedValue(obj map[string]any, loc []any) (any, bool) {
	c := ignoreCursor{obj, true}
	for _, e := range loc {
		switch e := e.(type) {
		case string:
			c = mapItem(c, e)
		case int:
			c = listItem(c, e)
		}
	}
	if !c.ok {
		return nil, false
	}
	return runtime.DeepCopyJSONValue(c.val), true
}

// setNestedValue sets the value at loc. It creates a missing map on the way,
// as unstructured.SetNestedField does, but it does not create or grow a list.
// It reports whether it set the value.
func setNestedValue(obj map[string]any, loc []any, val any) bool {
	if len(loc) == 0 {
		return false
	}
	var node any = obj
	for i, e := range loc {
		last := i == len(loc)-1
		switch e := e.(type) {
		case string:
			m, ok := node.(map[string]any)
			if !ok {
				return false
			}
			if last {
				m[e] = val
				return true
			}
			next, ok := m[e]
			if !ok {
				// Only a map can be created here: the next element decides.
				if _, isKey := loc[i+1].(string); !isKey {
					return false
				}
				next = map[string]any{}
				m[e] = next
			}
			node = next
		case int:
			l, ok := node.([]any)
			if !ok || e < 0 || e >= len(l) {
				return false
			}
			if last {
				l[e] = val
				return true
			}
			node = l[e]
		}
	}
	return false
}

// removeNestedValue removes the value at loc. A map loses the key, and a list
// loses the item.
func removeNestedValue(obj map[string]any, loc []any) {
	if len(loc) == 0 {
		return
	}
	removeNestedNode(obj, loc)
}

// removeNestedNode returns node without the value at loc. Removing an item
// makes a new list, so each level stores what the level below returns.
func removeNestedNode(node any, loc []any) any {
	switch e := loc[0].(type) {
	case string:
		m, ok := node.(map[string]any)
		if !ok {
			return node
		}
		if len(loc) == 1 {
			delete(m, e)
			return m
		}
		if child, ok := m[e]; ok {
			m[e] = removeNestedNode(child, loc[1:])
		}
		return m
	case int:
		l, ok := node.([]any)
		if !ok || e < 0 || e >= len(l) {
			return node
		}
		if len(loc) == 1 {
			out := make([]any, 0, len(l)-1)
			out = append(out, l[:e]...)
			return append(out, l[e+1:]...)
		}
		l[e] = removeNestedNode(l[e], loc[1:])
		return l
	}
	return node
}
