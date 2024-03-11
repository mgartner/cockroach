// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package idxconstraint

import (
	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/constraint"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/norm"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/errors"
)

func BuildParameterized(
	requiredFilters memo.FiltersExpr,
	optionalFilters memo.FiltersExpr,
	notNullCols opt.ColSet,
	columns []opt.OrderingColumn,
	evalCtx *eval.Context,
	f *norm.Factory,
) (_ *constraint.PConstraint, ok bool) {
	var pb parameterizedBuilder
	return pb.build(requiredFilters, optionalFilters, notNullCols, columns, evalCtx, f)
}

type parameterizedBuilder struct {
	f       *norm.Factory
	md      *opt.Metadata
	evalCtx *eval.Context

	columns []opt.OrderingColumn

	// keyCols is the set of index key columns and contains the same set of
	// columns as present in the columns slice above.
	keyCols opt.ColSet

	//
	// notNullCols opt.ColSet
	//
	// // We pre-initialize the KeyContext for each suffix of the index columns.
	// keyCtx []constraint.KeyContext

	// TODO
	exprs []opt.Expr

	hasPlaceholder bool
}

func (pb *parameterizedBuilder) build(
	requiredFilters memo.FiltersExpr,
	optionalFilters memo.FiltersExpr,
	notNullCols opt.ColSet,
	columns []opt.OrderingColumn,
	evalCtx *eval.Context,
	f *norm.Factory,
) (_ *constraint.PConstraint, ok bool) {
	var keyCols opt.ColSet
	for _, col := range columns {
		keyCols.Add(col.ID())
	}

	// This initialization pattern ensures that fields are not unwittingly
	// reused. Field reuse must be explicit.
	*pb = parameterizedBuilder{
		f:       f,
		md:      f.Metadata(),
		evalCtx: evalCtx,
		columns: columns,
		keyCols: keyCols,
		exprs:   make([]opt.Expr, len(columns)),
		// notNullCols: notNullCols,
		// keyCtx:      make([]constraint.KeyContext, len(columns)),
	}

	// Collect required filter expressions for each index column.
	if ok := pb.collectExprs(&requiredFilters); !ok {
		return nil, false
	}

	// A parameterized constraint requires at least one placeholder.
	if !pb.hasPlaceholder {
		return nil, false
	}

	// Determine if there are any constrained columns and any unconstrained
	// prefix columns.
	unconstrainedPrefix := false
	constrainedSuffix := false
	for i := range pb.exprs {
		unconstrainedPrefix = unconstrainedPrefix || pb.exprs[i] == nil
		if pb.exprs[i] != nil {
			constrainedSuffix = true
			break
		}
	}

	// If the required filters did not constrain any columns, then we cannot
	// build a parameterized constraint.
	if !constrainedSuffix {
		return nil, false
	}

	// TODO: Check for placeholder expressions.

	// If there are any unconstrained prefix columns, try to constrain them with
	// optional filters.
	if unconstrainedPrefix {
		pb.collectExprs(&optionalFilters)
	}

	// If there is not a prefix of constrained columns, then we cannot build a
	// parameterized constraint.
	if pb.exprs[0] == nil {
		return nil, false
	}

	// Build a parameterized constraint from the expressions.
	return pb.buildConstraint(), true
}

func (pb *parameterizedBuilder) collectExprs(e opt.Expr) (ok bool) {
	switch t := e.(type) {
	case *memo.EqExpr:
		return pb.collectEqExpr(t)
	case *memo.InExpr:
		return pb.collectInExpr(t)
	case *memo.AndExpr:
		return pb.collectExprs(t.Left) || pb.collectExprs(t.Right)
	case *memo.FiltersExpr:
		for i := range *t {
			if ok = pb.collectExprs((*t)[i].Condition); !ok {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (pb *parameterizedBuilder) collectEqExpr(e *memo.EqExpr) (ok bool) {
	indexColOrd, ok := pb.indexColumnOrd(e.Left)
	if !ok {
		return true
	}
	if pb.exprs[indexColOrd] != nil {
		// An expr was already set for this index column. Skip collection.
		return false
	}
	if opt.IsConstValueOp(e.Right) {
		pb.exprs[indexColOrd] = e.Right
		return
	}
	if _, ok := e.Right.(*memo.PlaceholderExpr); ok {
		pb.exprs[indexColOrd] = e.Right
		pb.hasPlaceholder = true
	}
	// TODO: Verify type.
	// TODO: Ignore NULL - they don't match things.
}

// TODO: Limit how many to collect (or to build) since there is a cross-product
// going on.
func (pb *parameterizedBuilder) collectInExpr(e *memo.InExpr) (ok bool) {
	indexColOrd, ok := pb.indexColumnOrd(e.Left)
	if !ok {
		return false
	}
	if pb.exprs[indexColOrd] != nil {
		// An expr was already set for this index column. Skip collection.
		return false
	}
	tup, ok := e.Right.(*memo.TupleExpr)
	if !ok {
		return false
	}
	// TODO: Make sure the tuple isn't empty... That would probably break
	// things. Add a test for this.
	// vals := make(tree.Datums, len(tup.Elems))
	for i := range tup.Elems {
		if !opt.IsConstValueOp(tup.Elems[i]) {
			return
		}
		// TODO: Verify type.
		// TODO: Ignore NULL - they don't match things.
	}
	pb.exprs[indexColOrd] = tup
}

// TODO: Must be called after
func (pb *parameterizedBuilder) buildConstraint() *constraint.PConstraint {
	// Find the last constrained column, and ensure that the
	keyCols := pb.columns
	for ord := range pb.exprs {
		if pb.exprs[ord] == nil {
			keyCols = pb.columns[:ord]
			break
		}
	}

	// Create a key context.
	var keyCtx constraint.KeyContext
	keyCtx.EvalCtx = pb.evalCtx
	keyCtx.Columns.Init(keyCols)

	keys := make([]tree.Datums, 1)
	keys[0] = make([]tree.Datum, len(keyCols))

	// duplicateKeys duplicates dupKeys into keys.
	duplicate := func(dupKeys []tree.Datums) {
		for i := 0; i < len(dupKeys); i++ {
			keyVals := make([]tree.Datum, len(keyCols))
			copy(keyVals, dupKeys[i])
			keys = append(keys, keyVals)
		}
	}

	for ord := range pb.exprs {
		switch t := pb.exprs[ord].(type) {
		case *memo.TupleExpr:
			keysToDup := keys[:]
			for k := range t.Elems {
				datum := memo.ExtractConstDatum(t.Elems[k])
				j := 0
				if k != 0 {
					// Create a new key for every value in the tuple. This
					// creates a cross-product of keys, duplicating all keys for
					// every tuple value.
					j = len(keys)
					duplicate(keysToDup)
				}
				// Fill the datum for all the duplicated keys.
				for ; j < len(keys); j++ {
					keys[j][ord] = datum
				}
			}

		default:
			for i := range keys {
				keys[i][ord] = extractDatum(t)
			}
		}
	}

	var spans constraint.Spans
	spans.Alloc(len(keys))
	for i := range keys {
		key := constraint.MakeCompositeKey(keys[i]...)
		var sp constraint.Span
		sp.Init(
			key, includeBoundary,
			key, includeBoundary,
		)
		spans.Append(&sp)
	}

	var c constraint.PConstraint
	c.Init(&keyCtx, &spans)
	return &c
}

// isIndexColumn returns true if e is an expression that corresponds to index
// column <offset>. The expression can be either
//   - a variable on the index column, or
//   - an expression that matches the computed column expression (if the index
//     column is computed).
//
// TODO: Document better.
//
// TODO(mgartner): Add support for expression index "columns".
func (pb *parameterizedBuilder) indexColumnOrd(e opt.Expr) (ord int, ok bool) {
	v, ok := e.(*memo.VariableExpr)
	if !ok {
		return 0, false
	}
	for i := range pb.columns {
		if pb.columns[i].ID() == v.Col {
			return i, true
		}
	}
	return 0, false
}

func extractDatum(e opt.Expr) tree.Datum {
	if opt.IsConstValueOp(e) {
		return memo.ExtractConstDatum(e)
	}
	if p, ok := e.(*memo.PlaceholderExpr); ok {
		return p.Value.(*tree.Placeholder)
	}
	panic(errors.AssertionFailedf("unexpected expression on RHS of '=': %T", e))
}
