// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package prep

import (
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"golang.org/x/exp/maps"
)

const (
	// The number of parallel test runs.
	numRuns = 1
	// The number of cache operations to perform per test run.
	numOps = 50
	// The number of distinct names to use.
	numNames = 50
	// The upperbound for choosing a random statement size.
	stmtSizeUpperbound = 20
	// The upperbound for choosing a random max size of the cache during
	// commits.
	maxSizeUpperbound = 100
)

func TestCache(t *testing.T) {
	t.Run("evict-none", func(t *testing.T) {
		var c Cache
		c.Add("s1", stmt(), 1)
		c.Add("s2", stmt(), 1)
		evicted := c.Commit(2)
		if len(evicted) > 0 {
			t.Errorf("expected no evictions")
		}
		c.Add("s3", stmt(), 10)
		assertContains(t, &c, "s1", "s2", "s3")
		c.Rewind()
		evicted = c.Commit(3)
		if len(evicted) > 0 {
			t.Errorf("expected no evictions")
		}
		evicted = c.Commit(2)
		if len(evicted) > 0 {
			t.Errorf("expected no evictions")
		}
		assertContains(t, &c, "s1", "s2")
	})

	t.Run("evict-committed", func(t *testing.T) {
		var c Cache
		c.Add("s1", stmt(), 1)
		c.Add("s2", stmt(), 1)
		c.Add("s3", stmt(), 1)
		_ = c.Commit(0)
		_, _ = c.Get("s1")
		evicted := c.Commit(2)
		if !slices.Equal(evicted, []string{"s2"}) {
			t.Errorf("expected s2 to be evicted, got %v", evicted)
		}
		assertContains(t, &c, "s1", "s3")
		c.Add("s4", stmt(), 5)
		evicted = c.Commit(5)
		if !slices.Equal(evicted, []string{"s3", "s1"}) {
			t.Errorf("expected s3 and s1 to be evicted, got %v", evicted)
		}
		assertContains(t, &c, "s4")
		evicted = c.Commit(4)
		if !slices.Equal(evicted, []string{"s4"}) {
			t.Errorf("expected s1 and s3 to be evicted, got %v", evicted)
		}
		// Cache should be empty.
		assertContains(t, &c)
	})

	t.Run("foo", func(t *testing.T) {
		var c Cache
		var o oracle
		o.init()

		c.Commit(2)
		c.Add("stmt_17", stmt(), 2)
		c.Remove("stmt_42")
		c.Commit(69)
		c.Commit(68)
		c.Commit(49)
		c.Rewind()
		c.Remove("stmt_40")
		c.Remove("stmt_14")
		c.Remove("stmt_7")
		c.Remove("stmt_4")
		c.Remove("stmt_38")
		c.Remove("stmt_44")
		c.Rewind()
		c.Add("stmt_44", stmt(), 13)
		c.Remove("stmt_7")
		c.Commit(82)
		c.Add("stmt_47", stmt(), 1)
		c.Remove("stmt_25")
		c.Add("stmt_32", stmt(), 7)
		c.Add("stmt_29", stmt(), 4)
		c.Remove("stmt_38")
		c.Remove("stmt_38")
		c.Add("stmt_47", stmt(), 8)
		c.Commit(46)
		c.Commit(74)
		c.Add("stmt_7", stmt(), 15)
		c.Remove("stmt_7")
		c.Remove("stmt_31")
		c.Add("stmt_45", stmt(), 3)

		o.Commit(2)
		o.Add("stmt_17", stmt(), 2)
		o.Remove("stmt_42")
		o.Commit(69)
		o.Commit(68)
		o.Commit(49)
		o.Rewind()
		o.Remove("stmt_40")
		o.Remove("stmt_14")
		o.Remove("stmt_7")
		o.Remove("stmt_4")
		o.Remove("stmt_38")
		o.Remove("stmt_44")
		o.Rewind()
		o.Add("stmt_44", stmt(), 13)
		o.Remove("stmt_7")
		o.Commit(82)
		o.Add("stmt_47", stmt(), 1)
		o.Remove("stmt_25")
		o.Add("stmt_32", stmt(), 7)
		o.Add("stmt_29", stmt(), 4)
		o.Remove("stmt_38")
		o.Remove("stmt_38")
		o.Add("stmt_47", stmt(), 8)
		o.Commit(46)
		o.Commit(74)
		o.Add("stmt_7", stmt(), 15)
		o.Remove("stmt_7")
		o.Remove("stmt_31")
		o.Add("stmt_45", stmt(), 3)

		cEvict := c.Commit(33)
		oEvict := o.Commit(33)

		if !slices.Equal(oEvict, cEvict) {
			t.Errorf("expected %v, got %v", oEvict, cEvict)
		}

		// --- FAIL: TestCache (0.00s)
		// --- FAIL: TestCache/random (0.00s)
		// --- FAIL: TestCache/random/0 (0.00s)
		// cache_test.go:128: expected evicted statements [stmt_14 stmt_46 stmt_38 stmt_17 stmt_7], got [stmt_38 stmt_14 stmt_46 stmt_17 stmt_7]
	})

	t.Run("evict-uncommitted", func(t *testing.T) {
		var c Cache
		c.Add("s1", stmt(), 2)
		c.Add("s2", stmt(), 2)
		c.Add("s3", stmt(), 2)
		c.Add("s4", stmt(), 2)
		_, _ = c.Get("s1")
		evicted := c.Commit(6)
		if !slices.Equal(evicted, []string{"s1"}) {
			// Evictions of uncommitted statements are not ordered by last Get.
			t.Errorf("expected s1 to be evicted, got: %v", evicted)
		}
		// Clear the cache.
		_ = c.Commit(1)
		c.Add("s5", stmt(), 2)
		c.Add("s6", stmt(), 2)
		c.Add("s7", stmt(), 2)
		c.Add("s8", stmt(), 2)
		evicted = c.Commit(2)
		if !slices.Equal(evicted, []string{"s5", "s6", "s7"}) {
			t.Errorf("expected s5, s6, and s7 to be evicted, got %v", evicted)
		}
		assertContains(t, &c, "s8")
	})

	t.Run("random", func(t *testing.T) {
		rng, _ := randutil.NewTestRand()
		for i := 0; i < numRuns; i++ {
			t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
				t.Parallel() // SAFE FOR TESTING (this comment is for the linter)
				var c Cache
				var o oracle
				o.init()
				for j := 0; j < numOps; j++ {
					n := rng.Intn(10)
					switch {
					case n == 0:
						maxSize := rng.Int63n(maxSizeUpperbound)
						cEvicted := c.Commit(maxSize)
						oEvicted := o.Commit(maxSize)
						fmt.Printf("c.Commit(%d)\n", maxSize)
						if !slices.Equal(cEvicted, oEvicted) {
							// t.Errorf("expected evicted statements %v, got %v", oEvicted, cEvicted)
							t.Fatalf("expected evicted statements %v, got %v", oEvicted, cEvicted)
						}
					case n == 1:
						fmt.Println("c.Rewind()")
						c.Rewind()
						o.Rewind()
					case n < 6:
						name := fmt.Sprintf("stmt_%d", rng.Intn(numNames))
						s := stmt()
						size := rng.Int63n(stmtSizeUpperbound)
						// TODO
						fmt.Printf("c.Add(%q, stmt(), %d)\n", name, size)
						c.Add(name, s, size)
						o.Add(name, s, size)
					default:
						name := fmt.Sprintf("stmt_%d", rng.Intn(numNames))
						// TODO
						fmt.Printf("c.Remove(%q)\n", name)
						c.Remove(name)
						o.Remove(name)
					}
					validate(t, &o, &c)
				}
			})
		}
	})
}

func stmt() *Statement { return new(Statement) }

func assertContains(t *testing.T, c *Cache, names ...string) {
	for _, name := range names {
		if !c.Has(name) {
			t.Errorf("expected cache to have %s", name)
		}
	}
	c.forEachWithUncommitted(func(name string, _ *Statement) {
		found := false
		for _, n := range names {
			if n == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("unexpected statement %s in cache", name)
		}
	})
}

func validate(t *testing.T, o *oracle, c *Cache) {
	// Check that every statement in the oracle is in the cache.
	o.forEachWithUncommitted(func(name string, stmt *Statement) {
		if !c.Has(name) {
			t.Errorf("expected c.Has to be true for %q", name)
		}
		// TODO: Do not Get here, do it above though.

		cStmt, ok := c.Get(name)
		if !ok {
			t.Errorf("expected c.Get to return ok=true for %q", name)
		}
		if stmt != cStmt {
			t.Errorf("c.Get returned incorrect statement")
		}
	})
	// Check that every statement in the cache is in the oracle.
	c.forEachWithUncommitted(func(name string, stmt *Statement) {
		if !o.Has(name) {
			t.Errorf("unexpected entry for %q in c", name)
		}
		// TODO: Do not Get here, do it above though.
		oStmt, ok := o.Get(name)
		if !ok {
			t.Errorf("unexpected entry for %q in c", name)
		}
		if stmt != oStmt {
			t.Errorf("c.Get returned incorrect statement")
		}
	})
}

// oracle implements the same API as Cache in a simpler and less efficient way.
// It is used for randomized tests for Cache.
type oracle struct {
	committed   map[string]oEntry
	uncommitted map[string]oEntry
	clock       int
}

type oEntry struct {
	stmt *Statement
	size int64
	t    int
}

func (o *oracle) init() {
	o.committed = make(map[string]oEntry)
	o.uncommitted = make(map[string]oEntry)
}

func (o *oracle) Add(name string, stmt *Statement, size int64) {
	o.uncommitted[name] = oEntry{stmt: stmt, size: size, t: o.clock}
	o.clock++
}

func (o *oracle) Remove(name string) {
	delete(o.uncommitted, name)
}

func (o *oracle) Has(name string) bool {
	_, ok := o.uncommitted[name]
	return ok
}

func (o *oracle) Get(name string) (stmt *Statement, ok bool) {
	e, ok := o.uncommitted[name]
	return e.stmt, ok
}

func (o *oracle) forEachWithUncommitted(fn func(name string, stmt *Statement)) {
	for name, e := range o.uncommitted {
		fn(name, e.stmt)
	}
}

func (o *oracle) Commit(maxSize int64) (evicted []string) {
	names := o.committedNamesAsc()
	// TODO fmt.Printf("names0: %v\n", names)
	for i := 0; i < len(names) && o.uncommittedSize() > maxSize; i++ {
		n := names[i].name
		delete(o.uncommitted, n)
		evicted = append(evicted, n)
	}
	maps.Clear(o.committed)
	maps.Copy(o.committed, o.uncommitted)
	names = o.committedNamesAsc()
	// TODO fmt.Printf("names1: %v\n", names)
	for i := 0; i < len(names) && o.committedSize() > maxSize; i++ {
		n := names[i].name
		delete(o.committed, n)
		delete(o.uncommitted, n)
		evicted = append(evicted, n)
	}
	return evicted
}

func (o *oracle) committedSize() int64 {
	var s int64
	for _, e := range o.committed {
		s += e.size
	}
	return s
}

func (o *oracle) uncommittedSize() int64 {
	var s int64
	for _, e := range o.uncommitted {
		s += e.size
	}
	return s
}

type orderedName struct {
	name string
	t    int
}

func (o *oracle) committedNamesAsc() []orderedName {
	var names []orderedName
	for n, e := range o.committed {
		names = append(names, orderedName{n, e.t})
	}
	sort.Slice(names, func(i, j int) bool {
		return names[i].t < names[j].t
	})
	return names
}

func (o *oracle) Rewind() {
	maps.Clear(o.uncommitted)
	maps.Copy(o.uncommitted, o.committed)
}
