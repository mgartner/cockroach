// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package prep

import "maps"

// Cache maps prepared statement names to Statements. In order to facilitate
// automatic retries which rewind the command buffer, Cache implements the
// Rewind method to restore the Cache to its state at the time of the last
// Commit.
//
// Cache is not thread-safe.
type Cache struct {
	m map[string]*Statement
	// tape contains uncommitted changes to the cache, in chronological order.
	tape []change
}

// change is an uncommitted mutation.
type change struct {
	name string
	stmt *Statement // nil if add=false
	add  bool       // true for additions, false for deletions
}

// Add adds a statement to the cache with the given name.
func (c *Cache) Add(name string, stmt *Statement) {
	c.tape = append(c.tape, change{name, stmt, true})
}

// Remove removes the statement with the given name from the cache.
// No-op if the name does not already exist in the cache.
func (c *Cache) Remove(name string) {
	c.tape = append(c.tape, change{name, nil, false})
}

// Has returns true if the prepared statement with the given name is in the
// cache.
func (c *Cache) Has(name string) bool {
	// First check the tape.
	for i := len(c.tape) - 1; i >= 0; i-- {
		if c.tape[i].name == name {
			if c.tape[i].add {
				return true
			} else {
				return false
			}
		}
	}
	// If nothing was found in the tape, check the map.
	_, ok := c.m[name]
	return ok
}

// Get looks up a prepared statement with the given name.
func (c *Cache) Get(name string) (stmt *Statement, ok bool) {
	// First check the tape.
	for i := len(c.tape) - 1; i >= 0; i-- {
		if c.tape[i].name == name {
			if c.tape[i].add {
				return c.tape[i].stmt, true
			} else {
				return nil, false
			}
		}
	}
	// If nothing was found in the tape, check the map.
	stmt, ok = c.m[name]
	return stmt, ok
}

// ForEach calls fn on every statement in the cache.
func (c *Cache) ForEach(fn func(name string, stmt *Statement)) {
	// Build a new map starting with the committed statements.
	m := make(map[string]*Statement)
	maps.Copy(m, c.m)
	// Apply the changes in the tape.
	for _, ch := range c.tape {
		if ch.add {
			m[ch.name] = ch.stmt
		} else {
			delete(m, ch.name)
		}
	}
	// Call fn on each statement.
	for name, stmt := range m {
		fn(name, stmt)
	}
}

// Commit makes permanent any changes to the cache via Add or Remove since the
// last Commit. Those changes can no longer be undone with Rewind.
func (c *Cache) Commit() {
	// Initialize the map if necessary.
	if c.m == nil {
		c.m = make(map[string]*Statement)
	}
	// Apply the tape to the map.
	for _, ch := range c.tape {
		if ch.add {
			c.m[ch.name] = ch.stmt
		} else {
			delete(c.m, ch.name)
		}
	}
	// Clear the tape.
	c.tape = c.tape[:0]
}

// Rewind returns the cache to its state at the previous Commit time.
func (c *Cache) Rewind() {
	// Clear the tape.
	c.tape = c.tape[:0]
}
