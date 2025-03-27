// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package prep

import (
	"fmt"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"golang.org/x/exp/maps"
)

const (
	// The number of parallel test runs.
	numRuns = 20
	// The number of cache operations to perform per test run.
	numOps = 5000
	// The number of distinct names to use.
	numNames = 50
)

func TestCache(t *testing.T) {
	rng, _ := randutil.NewTestRand()
	for i := 0; i < numRuns; i++ {
		t.Run(fmt.Sprintf("run%d", i), func(t *testing.T) {
			t.Parallel() // SAFE FOR TESTING (this comment is for the linter)
			var c Cache
			var o oracle
			o.init()
			for j := 0; j < numOps; j++ {
				n := rng.Intn(10)
				switch {
				case n == 0:
					c.Commit()
					o.Commit()
				case n == 1:
					c.Rewind()
					o.Rewind()
				case n < 6:
					stmt := new(Statement)
					name := fmt.Sprintf("stmt_%d", rng.Intn(numNames))
					c.Add(name, stmt)
					o.Add(name, stmt)
				default:
					name := fmt.Sprintf("stmt_%d", rng.Intn(numNames))
					c.Remove(name)
					o.Remove(name)
				}
				validate(t, &o, &c)
			}
		})
	}
}

func validate(t *testing.T, o *oracle, c *Cache) {
	// Check that every statement in the oracle is in the cache.
	o.ForEach(func(name string, stmt *Statement) {
		if !c.Has(name) {
			t.Fatalf("expected c.Has to be true for %q", name)
		}
		cStmt, ok := c.Get(name)
		if !ok {
			t.Fatalf("expected c.Get to return ok=true for %q", name)
		}
		if stmt != cStmt {
			t.Fatalf("c.Get returned incorrect statement")
		}
	})
	// Check that every statement in the cache is in the oracle.
	c.ForEach(func(name string, stmt *Statement) {
		if !o.Has(name) {
			t.Fatalf("unexpected entry for %q in c", name)
		}
		oStmt, ok := o.Get(name)
		if !ok {
			t.Fatalf("unexpected entry for %q in c", name)
		}
		if stmt != oStmt {
			t.Fatalf("c.Get returned incorrect statement")
		}
	})
}

// oracle implements the same API as Cache in a simpler and less efficient way.
// It is used for randomized tests for Cache.
type oracle struct {
	committed   map[string]*Statement
	uncommitted map[string]*Statement
}

func (o *oracle) init() {
	o.committed = make(map[string]*Statement)
	o.uncommitted = make(map[string]*Statement)
}

func (o *oracle) Add(name string, stmt *Statement) {
	o.uncommitted[name] = stmt
}

func (o *oracle) Remove(name string) {
	delete(o.uncommitted, name)
}

func (o *oracle) Has(name string) bool {
	_, ok := o.uncommitted[name]
	return ok
}

func (o *oracle) Get(name string) (stmt *Statement, ok bool) {
	stmt, ok = o.uncommitted[name]
	return stmt, ok
}

func (o *oracle) ForEach(fn func(name string, stmt *Statement)) {
	for name, stmt := range o.uncommitted {
		fn(name, stmt)
	}
}

func (o *oracle) Commit() {
	maps.Clear(o.committed)
	maps.Copy(o.committed, o.uncommitted)
}

func (o *oracle) Rewind() {
	maps.Clear(o.uncommitted)
	maps.Copy(o.uncommitted, o.committed)
}
