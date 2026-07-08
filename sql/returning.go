package sql

// returning.go: the RETURNING clause of INSERT, UPDATE, and DELETE.
// A RETURNING list is a single-table select list — resolved with
// SELECT's projection machinery against the target table's scope,
// then evaluated against each affected row instead of scanned ones.

import "slices"

// retProj is a resolved RETURNING list: projection entries over the
// table's columns plus any expression items evaluated per row.
type retProj struct {
	proj  []projEntry
	exprs []Expr
}

// resolveReturning resolves ret against env's single-table scope,
// appending the output column names and types to res. nil ret (no
// clause) resolves to nil.
func resolveReturning(env *exEnv, ret *Returning, res *Result) (*retProj, error) {
	if ret == nil {
		return nil, nil
	}
	proj, exprs, err := projectSelect(env.sc, &Select{Star: ret.Star, Items: ret.Items}, res)
	if err != nil {
		return nil, err
	}
	return &retProj{proj: proj, exprs: exprs}, nil
}

// row projects one affected table row (values in declared column
// order) into an output row.
func (rp *retProj) row(env *exEnv, vals []any) ([]any, error) {
	combined := vals
	if len(rp.exprs) > 0 {
		combined = slices.Clone(vals)
		for _, ex := range rp.exprs {
			rowEnv := *env
			rowEnv.row = vals
			v, err := evalEx(&rowEnv, ex)
			if err != nil {
				return nil, err
			}
			combined = append(combined, v)
		}
	}
	out := make([]any, len(rp.proj))
	for j, pe := range rp.proj {
		if pe.ord < 0 {
			out[j] = pe.lit
		} else {
			out[j] = combined[pe.ord]
		}
	}
	return out, nil
}
