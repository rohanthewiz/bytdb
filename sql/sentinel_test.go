package sql

// Deep-look D-56: the errors callers branch on match bytdb's sentinels
// under errors.Is, through the engine API and through SQL, and their
// messages name what went wrong (Postgres's wording) while the serr
// fields stay available.

import (
	"errors"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

func TestSentinelErrors(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table p (id int primary key, email text, n int not null default 0, check (n >= 0))`)
	exec(t, d, `create unique index p_email on p (email)`)
	exec(t, d, `create table c (id int primary key, pid int references p (id))`)
	exec(t, d, `insert into p values (1, 'a@x', 1)`)

	for _, c := range []struct {
		q     string
		is    []error
		isNot []error
		msg   string
	}{
		{`insert into p values (1, 'b@x', 1)`, []error{bytdb.ErrUniqueViolation, bytdb.ErrDuplicateKey}, nil, `duplicate key value violates unique constraint "p_pkey"`},
		{`insert into p values (2, 'a@x', 1)`, []error{bytdb.ErrUniqueViolation}, []error{bytdb.ErrDuplicateKey}, `duplicate key value violates unique constraint "p_email"`},
		{`insert into p values (null, 'z', 1)`, []error{bytdb.ErrNotNull}, nil, `null value in column "id" of relation "p" violates not-null constraint`},
		{`insert into p values (3, 'c@x', null)`, []error{bytdb.ErrNotNull}, nil, `violates not-null constraint`},
		{`insert into p values (4, 'd@x', -1)`, []error{bytdb.ErrCheck}, nil, `violates check constraint`},
		{`insert into c values (1, 99)`, []error{bytdb.ErrForeignKey}, nil, `violates foreign key constraint`},
		{`select * from nope`, []error{bytdb.ErrNoSuchTable}, nil, `relation "nope" does not exist`},
		{`select nope from p`, []error{bytdb.ErrNoSuchColumn}, nil, `column "nope" does not exist`},
		{`update p set nope = 1`, []error{bytdb.ErrNoSuchColumn}, nil, `column "nope" of relation "p" does not exist`},
		{`insrt into p values (1)`, nil, nil, `syntax error at or near "insrt"`},
		{`select * from`, nil, nil, `syntax error at end of input`},
	} {
		_, err := d.Exec(c.q)
		if err == nil {
			t.Errorf("%s: no error", c.q)
			continue
		}
		if !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: message %q, want containing %q", c.q, err.Error(), c.msg)
		}
		for _, s := range c.is {
			if !errors.Is(err, s) {
				t.Errorf("%s: errors.Is(%v) false", c.q, s)
			}
		}
		for _, s := range c.isNot {
			if errors.Is(err, s) {
				t.Errorf("%s: errors.Is(%v) true", c.q, s)
			}
		}
	}

	// The engine API classifies the same way, and the fields survive.
	if err := d.e.Insert("p", int64(1), "q@x", int64(1)); !errors.Is(err, bytdb.ErrDuplicateKey) {
		t.Errorf("Engine.Insert duplicate: %v", err)
	}
	_, _, err := d.e.Get("ghosts", int64(1))
	if !errors.Is(err, bytdb.ErrNoSuchTable) {
		t.Fatalf("Engine.Get unknown table: %v", err)
	}
	if got := bytdb.ErrText(err); !strings.Contains(got, "table: ghosts") {
		t.Errorf("ErrText lost the fields: %q", got)
	}
	if loc := serr.SErrFromErr(err).FieldsMap()["location"]; strings.Contains(loc, "err.go") {
		t.Errorf("location points at the constructor, not the raising site: %s", loc)
	}
	// A syntax error keeps its position field for the wire's caret.
	_, err = d.Exec(`select 1 frm p`)
	if serr.SErrFromErr(err).FieldsMap()["pos"] == "" {
		t.Errorf("syntax error lost pos: %v", serr.SErrFromErr(err).Fields())
	}

	// Kind lets an embedder raise errors the same callers recognize.
	custom := bytdb.Kind("quota exceeded", bytdb.ErrCheck)
	if !errors.Is(custom, bytdb.ErrCheck) || custom.Error() != "quota exceeded" {
		t.Errorf("Kind: %v", custom)
	}
}
