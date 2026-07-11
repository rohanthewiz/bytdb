package sql

import (
	"math"
	"strconv"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// sequence.go: standalone sequences — CREATE/DROP/ALTER SEQUENCE and
// the nextval/setval functions over the engine's sequence objects.
// Option defaulting follows Postgres: the declared AS type bounds the
// value range, an ascending sequence defaults to [1, typemax] starting
// at 1, a descending one to [typemin, -1] starting at -1. nextval and
// setval are session-recorded, so lastval() and currval('name') read
// them back exactly like identity draws.

// seqTypeBounds is the value range of a sequence's declared type.
func seqTypeBounds(asType string) (int64, int64) {
	switch asType {
	case "smallint":
		return math.MinInt16, math.MaxInt16
	case "integer":
		return math.MinInt32, math.MaxInt32
	}
	return math.MinInt64, math.MaxInt64
}

// checkSeqTypeRange enforces that declared bounds fit the declared
// type, with Postgres' wording.
func checkSeqTypeRange(d *bytdb.SeqDesc) error {
	tmin, tmax := seqTypeBounds(d.Type)
	if d.Min < tmin {
		return serr.New("MINVALUE (" + fmtI(d.Min) + ") is out of range for sequence data type " + seqTypeName(d.Type))
	}
	if d.Max > tmax {
		return serr.New("MAXVALUE (" + fmtI(d.Max) + ") is out of range for sequence data type " + seqTypeName(d.Type))
	}
	return nil
}

func seqTypeName(t string) string {
	if t == "" {
		return "bigint"
	}
	return t
}

// buildSeqDesc resolves a CREATE SEQUENCE's options into a full
// descriptor, applying the Postgres defaults.
func buildSeqDesc(s *CreateSequence) (*bytdb.SeqDesc, error) {
	d := &bytdb.SeqDesc{Name: s.Name, Type: s.Opts.AsType, Increment: 1, Cache: 1}
	if s.Opts.Increment != nil {
		d.Increment = *s.Opts.Increment
	}
	tmin, tmax := seqTypeBounds(d.Type)
	if d.Increment >= 0 { // ==0 errors in the engine's validation
		d.Min, d.Max = 1, tmax
	} else {
		d.Min, d.Max = tmin, -1
	}
	if s.Opts.Min != nil {
		d.Min = *s.Opts.Min
	}
	if s.Opts.Max != nil {
		d.Max = *s.Opts.Max
	}
	switch {
	case s.Opts.Start != nil:
		d.Start = *s.Opts.Start
	case d.Increment >= 0:
		d.Start = d.Min
	default:
		d.Start = d.Max
	}
	if s.Opts.Cache != nil {
		d.Cache = *s.Opts.Cache
	}
	if s.Opts.Cycle != nil {
		d.Cycle = *s.Opts.Cycle
	}
	if err := checkSeqTypeRange(d); err != nil {
		return nil, err
	}
	return d, nil
}

// applySeqOptions maps an ALTER SEQUENCE's options onto the stored
// descriptor. Explicit NO MINVALUE / NO MAXVALUE reset the bound to
// its default for the (possibly just-changed) increment direction and
// type; RESTART repositions the sequence the way its creation did.
func applySeqOptions(d *bytdb.SeqDesc, o SeqOptions) error {
	if o.AsType != "" {
		d.Type = o.AsType
	}
	if o.Increment != nil {
		d.Increment = *o.Increment
	}
	tmin, tmax := seqTypeBounds(d.Type)
	if o.Min != nil {
		d.Min = *o.Min
	} else if o.NoMin {
		if d.Increment >= 0 {
			d.Min = 1
		} else {
			d.Min = tmin
		}
	}
	if o.Max != nil {
		d.Max = *o.Max
	} else if o.NoMax {
		if d.Increment >= 0 {
			d.Max = tmax
		} else {
			d.Max = -1
		}
	}
	if o.Start != nil {
		d.Start = *o.Start
	}
	if o.Cache != nil {
		d.Cache = *o.Cache
	}
	if o.Cycle != nil {
		d.Cycle = *o.Cycle
	}
	if err := checkSeqTypeRange(d); err != nil {
		return err
	}
	if o.Restart {
		v := d.Start
		if o.RestartWith != nil {
			v = *o.RestartWith
		}
		if v < d.Min {
			return serr.New("RESTART value (" + fmtI(v) + ") cannot be less than MINVALUE (" + fmtI(d.Min) + ")")
		}
		if v > d.Max {
			return serr.New("RESTART value (" + fmtI(v) + ") cannot be greater than MAXVALUE (" + fmtI(d.Max) + ")")
		}
		d.Last, d.Called = v, false
	}
	return nil
}

func (d *DB) execCreateSequence(s *CreateSequence) (*Result, error) {
	if s.IfNotExists && d.e.Sequence(s.Name) != nil {
		return &Result{Notice: `relation "` + s.Name + `" already exists, skipping`}, nil
	}
	desc, err := buildSeqDesc(s)
	if err != nil {
		return nil, err
	}
	if err := d.e.CreateSequence(desc); err != nil {
		return nil, err
	}
	return &Result{}, nil
}

func (d *DB) execDropSequence(s *DropSequence) (*Result, error) {
	existed, err := d.e.DropSequence(s.Name)
	if err != nil {
		return nil, err
	}
	if !existed {
		if s.IfExists {
			return &Result{Notice: `sequence "` + s.Name + `" does not exist, skipping`}, nil
		}
		return nil, serr.New(`sequence "` + s.Name + `" does not exist`)
	}
	return &Result{}, nil
}

func (d *DB) execAlterSequence(s *AlterSequence) (*Result, error) {
	// Match Postgres' error class for a missing sequence (the engine
	// would say "relation" — psql scripts test for "sequence").
	if d.e.Sequence(s.Name) == nil {
		return nil, serr.New(`sequence "` + s.Name + `" does not exist`)
	}
	err := d.e.AlterSequence(s.Name, func(sd *bytdb.SeqDesc) error {
		return applySeqOptions(sd, s.Opts)
	})
	if err != nil {
		return nil, err
	}
	return &Result{}, nil
}

// seqByArg resolves nextval/setval's first argument: a sequence name,
// or an oid (int64) from a 'name'::regclass cast, which is how psql
// and drivers commonly spell it.
func seqByArg(env *exEnv, v any) (string, error) {
	switch a := v.(type) {
	case string:
		return a, nil
	case int64:
		for _, sd := range env.d.e.Sequences() {
			if int64(sd.ID) == a {
				return sd.Name, nil
			}
		}
		return "", serr.New("no sequence with the given oid", "oid", fmtI(a))
	}
	return "", serr.New("a sequence name is required")
}

// selectWritesSequences reports whether any expression of the SELECT
// (subqueries and UNION arms included) calls nextval or setval — such
// a query must run in a writable transaction even though it is a
// SELECT, since drawing a value writes the sequence record.
func selectWritesSequences(s *Select) bool {
	if s == nil {
		return false
	}
	for _, it := range s.Items {
		if exprWritesSequences(it.Ex) {
			return true
		}
	}
	for _, o := range s.OrderBy {
		if exprWritesSequences(o.Ex) {
			return true
		}
	}
	for _, g := range s.GroupBy {
		if exprWritesSequences(g.Ex) {
			return true
		}
	}
	if boolWritesSequences(s.Where) || boolWritesSequences(s.Having) {
		return true
	}
	for _, f := range s.From {
		if boolWritesSequences(f.On) {
			return true
		}
	}
	for _, u := range s.Union {
		if selectWritesSequences(u.Sel) {
			return true
		}
	}
	return false
}

func exprWritesSequences(e Expr) bool {
	found := false
	walkExpr(e, func(sub Expr) bool {
		switch n := sub.(type) {
		case *ExFunc:
			if n.Name == "nextval" || n.Name == "setval" {
				found = true
			}
		case *ExSub:
			if selectWritesSequences(n.Sel) {
				found = true
			}
		}
		return !found
	})
	return found
}

func boolWritesSequences(b BoolExpr) bool {
	switch n := b.(type) {
	case *Pred:
		return exprWritesSequences(n.Item.Ex) ||
			(n.RItem != nil && exprWritesSequences(n.RItem.Ex))
	case *Cond:
		return exprWritesSequences(n.Ex)
	case *Not:
		return boolWritesSequences(n.Expr)
	case *And:
		for _, sub := range n.Exprs {
			if boolWritesSequences(sub) {
				return true
			}
		}
	case *Or:
		for _, sub := range n.Exprs {
			if boolWritesSequences(sub) {
				return true
			}
		}
	}
	return false
}

// fmtI renders an int64 for sequence error messages.
func fmtI(v int64) string {
	return strconv.FormatInt(v, 10)
}
