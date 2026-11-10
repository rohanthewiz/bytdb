package sql

// parsecache.go: an LRU of parsed statements keyed by SQL text
// (deep-look D-40 step 4).
//
// Every unprepared statement — DB.Exec, a Session's statement, each
// statement of a wire simple-protocol query, each wire Parse message
// (unnamed statements are re-parsed per execution by most drivers) —
// lexed and parsed its text from scratch, about 29% of an unprepared
// point read. Applications send the same few texts over and over, so
// the parsed tree is kept and shared.
//
// Sharing is sound because execution never writes to a parsed tree:
// parameters bind into a copy (bindParams), typing rebuilds only the
// nodes it changes (typeStatement), and every per-execution cache lives
// in the execution's own memo — the same property a prepared Stmt,
// executed concurrently, already relies on. TestParseCacheTreesStayPristine
// checks it for the statement kinds cached here; DDL, transaction
// control and the rest are cheap to parse, rarely repeated in a hot
// loop, and not cached.

import (
	"container/list"
	"sync"
)

// parseCacheSize bounds the number of cached statements per DB (shared
// by its sessions). maxCachedQueryLen keeps one giant generated query
// (a 10,000-row INSERT) from pinning megabytes of tree for nothing: a
// text that long is rarely sent twice.
const (
	parseCacheSize    = 512
	maxCachedQueryLen = 8 << 10
)

type parseCache struct {
	mu  sync.Mutex
	ll  *list.List // front: most recently used; values are *parseEntry
	ent map[string]*list.Element
}

type parseEntry struct {
	query string
	st    Statement
}

func newParseCache() *parseCache {
	return &parseCache{ll: list.New(), ent: map[string]*list.Element{}}
}

// parse returns query's parsed statement, from the cache when it holds
// one. A nil cache parses every time. Parse errors are not cached.
func (c *parseCache) parse(query string) (Statement, error) {
	if c == nil || len(query) > maxCachedQueryLen {
		return Parse(query)
	}
	c.mu.Lock()
	if el, ok := c.ent[query]; ok {
		c.ll.MoveToFront(el)
		st := el.Value.(*parseEntry).st
		c.mu.Unlock()
		return st, nil
	}
	c.mu.Unlock()

	st, err := Parse(query)
	if err != nil || !cacheableStatement(st) {
		return st, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.ent[query]; ok { // another session parsed it meanwhile
		c.ll.MoveToFront(el)
		return el.Value.(*parseEntry).st, nil
	}
	c.ent[query] = c.ll.PushFront(&parseEntry{query: query, st: st})
	if c.ll.Len() > parseCacheSize {
		old := c.ll.Back()
		c.ll.Remove(old)
		delete(c.ent, old.Value.(*parseEntry).query)
	}
	return st, nil
}

// cacheableStatement reports whether a statement kind may be shared
// across executions from the cache: the DML and query kinds, whose
// trees execution is checked never to modify.
func cacheableStatement(st Statement) bool {
	switch st.(type) {
	case *Select, *Insert, *Update, *Delete:
		return true
	}
	return false
}

// parse parses query through the DB's statement cache.
func (d *DB) parse(query string) (Statement, error) {
	return d.parses.parse(query)
}
