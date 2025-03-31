// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package prep

import (
	"fmt"
	"maps"
)

// Cache maps prepared statement names to Statements. In order to facilitate
// automatic retries which rewind the command buffer, Cache implements the
// Rewind method to restore the Cache to its state at the time of the last
// Commit.
//
// Cache applies an LRU eviction policy during Commit. See Commit for more
// details.
//
// Cache is not thread-safe.
type Cache struct {
	m map[string]*entry
	// tape contains only uncommitted statements in the cache, in chronological
	// order.
	tape []change
	// lru contains only committed statements in the cache.
	lru       list
	cacheSize int64
	tapeSize  int64
}

type entry struct {
	name       string
	stmt       *Statement
	prev, next *entry
	size       int64
}

// change is an uncommitted mutation.
// TODO: Maybe just use entry and List instead for the tape?
type change struct {
	name string
	stmt *Statement // nil if add=false
	add  bool       // true for additions, false for deletions
	size int64
}

// Add adds a statement to the cache with the given name.
func (c *Cache) Add(name string, stmt *Statement, size int64) {
	c.tape = append(c.tape, change{name, stmt, true, size})
	c.tapeSize += size
}

// Remove removes the statement with the given name from the cache. No-op if the
// name does not already exist in the cache.
func (c *Cache) Remove(name string) {
	_, size, _, ok := c.get(name)
	if !ok {
		return
	}
	c.tape = append(c.tape, change{name, nil, false, size})
	c.tapeSize -= size
}

// Has returns true if the prepared statement with the given name is in the
// cache. It is not considered an access and therefore does not alter the LRU
// ordering.
func (c *Cache) Has(name string) bool {
	_, _, _, ok := c.get(name)
	return ok
}

// Get looks up a prepared statement with the given name. It is considered an
// access so it updates the LRU ordering if the statement was previously
// committed.
func (c *Cache) Get(name string) (stmt *Statement, ok bool) {
	stmt, _, e, ok := c.get(name)
	// Push e to the front of the lru. If the statement was found in the tape, e
	// is nil and remove and push are no-ops, so the LRU ordering is not
	// altered.
	c.lru.remove(e)
	c.lru.push(e)
	return stmt, ok
}

// get looks up a prepared statement with the given name. It returns the
// statement and its size. It also returns its entry in the cache if it is
// committed. It does not alter the LRU ordering.
func (c *Cache) get(name string) (stmt *Statement, size int64, e *entry, ok bool) {
	// First check the tape.
	for i := len(c.tape) - 1; i >= 0; i-- {
		if c.tape[i].name == name {
			if c.tape[i].add {
				return c.tape[i].stmt, c.tape[i].size, nil, true
			} else {
				return nil, 0, nil, false
			}
		}
	}
	// If nothing was found in the tape, check the map.
	if e, ok := c.m[name]; ok {
		return e.stmt, e.size, e, true
	}
	return nil, 0, nil, false
}

// Commit makes permanent any changes to the cache via Add or Remove since the
// last Commit. Those changes can no longer be undone with Rewind.
//
// If maxSize is positive, Commit will ensure that the size of the cache does
// not exceed it. It does this by first evicting previously committed
// statements, in an LRU order based on the time of the Add or the last Get. If
// the size of the statements in the tape are larger than maxSize, then
// uncommitted statements are evicted (i.e., never committed), also with in an
// LRU order.
func (c *Cache) Commit(maxSize int64) (evicted []string) {
	// Initialize the map and lru if necessary.
	if c.m == nil {
		c.m = make(map[string]*entry)
		c.lru.init()
	}
	// Make room for the changes in the tape, if necessary.
	evicted = c.evict(maxSize, evicted)
	// Apply the tape to the map and list.
	for _, ch := range c.tape {
		if ch.add {
			e := &entry{name: ch.name, stmt: ch.stmt, size: ch.size}
			c.m[ch.name] = e
			c.lru.push(e)
			c.cacheSize += e.size
		} else {
			if e, ok := c.m[ch.name]; ok {
				delete(c.m, ch.name)
				c.lru.remove(e)
				c.cacheSize -= e.size
			}
		}
	}
	// Clear the tape.
	c.tape = c.tape[:0]
	c.tapeSize = 0
	// The size of the tape may have been larger than maxSize, so run eviction
	// again.
	evicted = c.evict(maxSize, evicted)
	return evicted
}

// evict removes committed statements from the cache until the size of the
// committed and uncommitted statements is less than or equal to maxSize, or
// until there are no more committed statements. It appends the names of the
// removed statements to evicted and returns the updated slice. evict is a no-op
// if maxSize is not positive.
func (c *Cache) evict(maxSize int64, evicted []string) []string {
	if maxSize <= 0 {
		return nil
	}
	for e := c.lru.tail(); e != nil && c.cacheSize+c.tapeSize > maxSize; e = c.lru.prev(e) {
		fmt.Printf("cacheSize: %d, tapeSize: %d\n", c.cacheSize, c.tapeSize)
		delete(c.m, e.name)
		c.lru.remove(e)
		c.cacheSize -= e.size
		evicted = append(evicted, e.name)
	}
	return evicted
}

// Rewind returns the cache to its state at the previous Commit time.
func (c *Cache) Rewind() {
	for i := range c.tape {
		// Clear references.
		c.tape[i] = change{}
	}
	c.tape = c.tape[:0]
	c.tapeSize = 0
}

// ForEach calls fn on every committed statement in the cache in ascending order
// of the last access with Get. Uncommitted statements are excluded.
func (c *Cache) ForEach(fn func(name string, stmt *Statement)) {
	// Call fn on each committed statement.
	for e := c.lru.tail(); e != nil; e = c.lru.prev(e) {
		fn(e.name, e.stmt)
	}
}

// forEachWithUncommitted calls fn on every statement in the cache. Uncommitted
// statements are included. There is no guaranteed ordering of statements.
func (c *Cache) forEachWithUncommitted(fn func(name string, stmt *Statement)) {
	// Build a new map starting with the committed statements.
	m := make(map[string]*entry)
	maps.Copy(m, c.m)
	// Apply the changes in the tape.
	for _, ch := range c.tape {
		if ch.add {
			m[ch.name] = &entry{name: ch.name, stmt: ch.stmt}
		} else {
			delete(m, ch.name)
		}
	}
	// Call fn on each statement.
	for name, e := range m {
		fn(name, e.stmt)
	}
}

// list is a doubly-linked list of entries.
type list struct {
	// root is a sentinel value. root.next and root.prev are the head and the
	// tail.
	root entry
}

// init initializes a list.
func (l *list) init() {
	l.root.next = &l.root
	l.root.prev = &l.root
}

// tail returns the last entry in the list or nil if the list is empty.
func (l *list) tail() *entry {
	if l.root.prev == &l.root {
		return nil
	}
	return l.root.prev
}

// prev returns the entry in the list before e or nil if one does not exist.
func (l *list) prev(e *entry) *entry {
	if l.root.prev == &l.root {
		return nil
	}
	return l.root.prev
}

// push pushes the entry onto the front of the list. The entry must not already
// be in the list.
func (l *list) push(e *entry) {
	if e == nil {
		return
	}
	h := l.root.next
	e.next, e.prev = h, h.prev
	h.prev = e
	l.root.next = e
}

// remove removes the entry from the list. The entry must already be in the list.
func (l *list) remove(e *entry) {
	if e == nil {
		return
	}
	e.prev.next = e.next
	e.next.prev = e.prev
	e.next = nil
	e.prev = nil
}
