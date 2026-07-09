package sql

// seqfuncs.go: lastval() and currval(), the session-scoped sequence
// readbacks some drivers probe after an insert. bytdb has no
// user-visible sequences; identity columns are what draw values, so
// each draw records under the column's implied sequence name —
// table_column_seq, the same name information_schema.columns reports
// in column_default. As in Postgres, the readbacks survive a rolled-
// back block: nextval state is session-local, not transactional.

import (
	"sync"

	"github.com/rohanthewiz/serr"
)

// seqSession is one session's sequence-function state. Each Session
// owns one; a bare DB acts as a single session. The mutex covers the
// bare-DB case, where Exec may run from many goroutines.
type seqSession struct {
	mu   sync.Mutex
	last int64
	ok   bool
	curr map[string]int64
}

// record notes one identity draw under its implied sequence name.
func (s *seqSession) record(name string, v int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last, s.ok = v, true
	if s.curr == nil {
		s.curr = map[string]int64{}
	}
	s.curr[name] = v
}

// lastval is the most recent identity value drawn in this session,
// with Postgres's wording (SQLSTATE 55000 over the wire) before the
// first draw.
func (s *seqSession) lastval() (any, error) {
	if s == nil {
		return nil, serr.New("lastval is not yet defined in this session")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ok {
		return nil, serr.New("lastval is not yet defined in this session")
	}
	return s.last, nil
}

// currval is the most recent value this session drew for the named
// sequence. A name nothing has drawn from reports "not yet defined"
// whether or not the sequence exists — sequences are implied by
// identity columns here, not cataloged objects to look up.
func (s *seqSession) currval(name string) (any, error) {
	if s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if v, ok := s.curr[name]; ok {
			return v, nil
		}
	}
	return nil, serr.New(`currval of sequence "` + name + `" is not yet defined in this session`)
}

// identitySeqName is the implied sequence name of an identity column,
// matching the nextval('...') default information_schema reports.
func identitySeqName(table, column string) string {
	return table + "_" + column + "_seq"
}
