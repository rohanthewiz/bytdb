package sql

// jsonb_ops.go: the jsonb operator family over TJSONB values. A jsonb
// runtime value is its canonical text (see bytdb.CanonJSONB), so every
// operator here is parse → operate on Go shapes → re-render. The Go
// shapes are what encoding/json produces with UseNumber: map[string]any,
// []any, string, json.Number, bool, nil.
//
// Two operator kinds, mirroring how the rest of the layer splits work:
//
//   - value operators (-> ->> #> #>> || -) evaluate inside expressions
//     via jsonbArith, dispatched from evalEx's ExArith case;
//   - boolean operators (@> <@ ? ?| ?&) are PredOps evaluated by
//     checkPred via jsonbPredicate, so they ride the Pred machinery
//     (three-valued logic, residual filters) unchanged.
//
// Postgres's error/NULL split is kept: accessors answer NULL for any
// miss (absent key, index out of range, wrong container kind), while
// deletion from the wrong container kind is an error.

import (
	"encoding/json"
	"io"
	"maps"
	"math/big"
	"strconv"
	"strings"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// parseJSONB decodes a jsonb runtime value into Go shapes. UseNumber
// keeps numeric source text (json.Number), matching CanonJSONB, so a
// re-render reproduces the canonical spelling byte for byte.
func parseJSONB(s string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, serr.New("invalid input syntax for type json", "value", s)
	}
	// One document only, as in CanonJSONB; operator operands normally
	// arrive canonical, but a text literal reaches here unvalidated.
	if _, err := dec.Token(); err != io.EOF {
		return nil, serr.New("invalid input syntax for type json", "value", s)
	}
	return v, nil
}

// renderJSONB is parseJSONB's inverse: the canonical text of a decoded
// document. Go's encoder sorts map keys lexically — the same order
// CanonJSONB emits — so extracted sub-documents compare equal to
// independently stored copies of the same value.
func renderJSONB(v any) (string, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", serr.Wrap(err, "op", "render jsonb")
	}
	// Encode terminates with a newline the canonical form must not carry.
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// jsonbArith applies a jsonb value operator; l is the jsonb operand's
// canonical text, r the operator-specific right side (text key, integer
// index, text[] path, or jsonb text). Both operands were checked
// non-NULL by the caller (the operators are NULL-strict, like the rest
// of ExArith).
func jsonbArith(op string, l, r any) (any, error) {
	ls, ok := l.(string)
	if !ok {
		return nil, serr.New("jsonb operator requires a jsonb left operand", "op", op)
	}
	doc, err := parseJSONB(ls)
	if err != nil {
		return nil, err
	}
	switch op {
	case "->", "->>":
		sub, found := jsonbGet(doc, r)
		if !found {
			return nil, nil
		}
		if op == "->>" {
			return jsonbAsText(sub)
		}
		return renderJSONB(sub)
	case "#>", "#>>":
		rs, ok := r.(string)
		if !ok {
			return nil, serr.New("jsonb path operator requires a text[] right operand", "op", op)
		}
		path, err := bytdb.ParseTextArray(rs)
		if err != nil {
			return nil, err
		}
		sub, found := jsonbGetPath(doc, path)
		if !found {
			return nil, nil
		}
		if op == "#>>" {
			return jsonbAsText(sub)
		}
		return renderJSONB(sub)
	case "||":
		rs, ok := r.(string)
		if !ok {
			return nil, serr.New("jsonb || requires a jsonb right operand")
		}
		other, err := parseJSONB(rs)
		if err != nil {
			return nil, err
		}
		return renderJSONB(jsonbConcatVals(doc, other))
	case "-":
		out, err := jsonbDeleteVal(doc, r)
		if err != nil {
			return nil, err
		}
		return renderJSONB(out)
	}
	return nil, serr.New("internal: not a jsonb value operator", "op", op)
}

// jsonbGet resolves one -> step: a text key against an object or an
// integer index against an array (negative counts from the end). Any
// other pairing — including a text key against an array, mirroring
// Postgres's operator resolution — misses rather than errors.
func jsonbGet(doc any, key any) (any, bool) {
	switch k := key.(type) {
	case string:
		if m, ok := doc.(map[string]any); ok {
			v, ok := m[k]
			return v, ok
		}
	case int64:
		if a, ok := doc.([]any); ok {
			i := k
			if i < 0 {
				i += int64(len(a))
			}
			if i >= 0 && i < int64(len(a)) {
				return a[i], true
			}
		}
	}
	return nil, false
}

// jsonbGetPath walks a #> path: each element is an object key, or —
// when the current level is an array — the decimal text of an index.
// A step that does not resolve (including a NULL path element or
// non-numeric text against an array) misses, it never errors.
func jsonbGetPath(doc any, path []any) (any, bool) {
	cur := doc
	for _, pe := range path {
		key, ok := pe.(string)
		if !ok {
			return nil, false
		}
		switch c := cur.(type) {
		case map[string]any:
			v, ok := c[key]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			n, err := strconv.ParseInt(strings.TrimSpace(key), 10, 64)
			if err != nil {
				return nil, false
			}
			if n < 0 {
				n += int64(len(c))
			}
			if n < 0 || n >= int64(len(c)) {
				return nil, false
			}
			cur = c[n]
		default:
			return nil, false
		}
	}
	return cur, true
}

// jsonbAsText is the ->> / #>> result form: a JSON string yields its
// content (no quotes), JSON null yields SQL NULL, and containers and
// other scalars yield their canonical text.
func jsonbAsText(v any) (any, error) {
	switch s := v.(type) {
	case nil:
		return nil, nil
	case string:
		return s, nil
	}
	return renderJSONB(v)
}

// jsonbConcatVals implements jsonb ||. Two objects merge with the
// right side winning key collisions; any other pairing concatenates as
// arrays, wrapping a non-array side as a one-element array — exactly
// Postgres's rule set.
func jsonbConcatVals(l, r any) any {
	if lm, ok := l.(map[string]any); ok {
		if rm, ok := r.(map[string]any); ok {
			out := make(map[string]any, len(lm)+len(rm))
			maps.Copy(out, lm)
			maps.Copy(out, rm) // right side wins key collisions
			return out
		}
	}
	la, ok := l.([]any)
	if !ok {
		la = []any{l}
	}
	ra, ok := r.([]any)
	if !ok {
		ra = []any{r}
	}
	out := make([]any, 0, len(la)+len(ra))
	return append(append(out, la...), ra...)
}

// jsonbDeleteVal implements jsonb - text (drop an object key, or every
// equal string element of an array) and jsonb - integer (drop the
// array element at an index, negative from the end; out of range is a
// no-op). Deleting from the wrong container kind errors, in Postgres's
// words — silently returning the input would hide a type mistake.
func jsonbDeleteVal(doc any, key any) (any, error) {
	switch k := key.(type) {
	case string:
		switch c := doc.(type) {
		case map[string]any:
			out := make(map[string]any, len(c))
			for mk, mv := range c {
				if mk != k {
					out[mk] = mv
				}
			}
			return out, nil
		case []any:
			out := make([]any, 0, len(c))
			for _, e := range c {
				if s, ok := e.(string); ok && s == k {
					continue
				}
				out = append(out, e)
			}
			return out, nil
		}
		return nil, serr.New("cannot delete from scalar")
	case int64:
		a, ok := doc.([]any)
		if !ok {
			if _, isObj := doc.(map[string]any); isObj {
				return nil, serr.New("cannot delete from object using integer index")
			}
			return nil, serr.New("cannot delete from scalar")
		}
		i := k
		if i < 0 {
			i += int64(len(a))
		}
		if i < 0 || i >= int64(len(a)) {
			return a, nil
		}
		out := make([]any, 0, len(a)-1)
		out = append(out, a[:i]...)
		return append(out, a[i+1:]...), nil
	}
	return nil, serr.New("jsonb - requires a text key or an integer index")
}

// jsonbPredicate evaluates a jsonb boolean operator for checkPred; doc
// is the left operand's canonical text, arg the right side (jsonb text
// for containment, a key or a '{...}' key list for existence).
func jsonbPredicate(op PredOp, doc, arg string) (bool, error) {
	d, err := parseJSONB(doc)
	if err != nil {
		return false, err
	}
	switch op {
	case OpContains, OpContainedBy:
		o, err := parseJSONB(arg)
		if err != nil {
			return false, err
		}
		if op == OpContainedBy {
			d, o = o, d
		}
		return jsonbContains(d, o, true), nil
	case OpKeyExists:
		return jsonbKeyExists(d, arg), nil
	case OpKeyExistsAny, OpKeyExistsAll:
		keys, err := bytdb.ParseTextArray(arg)
		if err != nil {
			return false, err
		}
		for _, k := range keys {
			s, ok := k.(string)
			hit := ok && jsonbKeyExists(d, s)
			if op == OpKeyExistsAny && hit {
				return true, nil
			}
			if op == OpKeyExistsAll && !hit {
				return false, nil
			}
		}
		// ?| over an empty list found nothing; ?& had nothing to fail.
		return op == OpKeyExistsAll, nil
	}
	return false, serr.New("internal: not a jsonb predicate")
}

// jsonbContains implements @>: does a contain b?
//
//   - object ⊇ object: every key of b exists in a with a *containing*
//     value — containment recurses through object values, so
//     {"a":{"x":1,"y":2}} contains {"a":{"x":1}};
//   - array ⊇ array: every element of b matches some element of a
//     (order and duplicates irrelevant) — containers match by
//     containment, scalars by equality, and kinds never cross, so
//     [1,2,[1,3]] does not contain [1,3] but does contain [[1,3]];
//   - scalar ⊇ scalar: equality;
//   - the one asymmetry: at the top level only, an array contains a
//     bare scalar that equals one of its elements ('[1,2]' @> '1').
func jsonbContains(a, b any, top bool) bool {
	switch bv := b.(type) {
	case map[string]any:
		av, ok := a.(map[string]any)
		if !ok {
			return false
		}
		for k, sub := range bv {
			as, ok := av[k]
			if !ok || !jsonbContains(as, sub, false) {
				return false
			}
		}
		return true
	case []any:
		av, ok := a.([]any)
		if !ok {
			return false
		}
		for _, be := range bv {
			found := false
			for _, ae := range av {
				if jsonbElemMatch(ae, be) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	default: // scalar
		if av, ok := a.([]any); ok && top {
			for _, ae := range av {
				if jsonbEqual(ae, b) {
					return true
				}
			}
			return false
		}
		return jsonbEqual(a, b)
	}
}

// jsonbElemMatch is the per-element rule of array containment: a
// container element of the needle matches a same-kind haystack element
// it is contained in; a scalar element matches by equality only.
func jsonbElemMatch(ae, be any) bool {
	switch be.(type) {
	case map[string]any:
		if _, ok := ae.(map[string]any); !ok {
			return false
		}
		return jsonbContains(ae, be, false)
	case []any:
		if _, ok := ae.([]any); !ok {
			return false
		}
		return jsonbContains(ae, be, false)
	}
	return jsonbEqual(ae, be)
}

// jsonbEqual is structural equality of decoded documents. Numbers
// compare numerically (big.Rat, exact at any magnitude), so 1 and 1.0
// are equal inside containment even though canonical text — and hence
// the = operator on jsonb columns — keeps their source spellings apart.
func jsonbEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, x := range av {
			y, ok := bv[k]
			if !ok || !jsonbEqual(x, y) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonbEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case json.Number:
		bv, ok := b.(json.Number)
		if !ok {
			return false
		}
		ar, aok := new(big.Rat).SetString(av.String())
		br, bok := new(big.Rat).SetString(bv.String())
		if aok && bok {
			return ar.Cmp(br) == 0
		}
		return av == bv // unparseable numbers cannot come from valid JSON
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case nil:
		return b == nil
	}
	return false
}

// jsonbKeyExists implements ?: an object has the key, an array has the
// string as an element, a string scalar is the string. Other scalars
// never match.
func jsonbKeyExists(doc any, key string) bool {
	switch c := doc.(type) {
	case map[string]any:
		_, ok := c[key]
		return ok
	case []any:
		for _, e := range c {
			if s, ok := e.(string); ok && s == key {
				return true
			}
		}
	case string:
		return c == key
	}
	return false
}
