// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package xform

import (
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/volatility"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondatapb"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/errors"
)

// GenericRulesEnabled returns true if rules for optimizing generic query plans
// are enabled, based on the plan_cache_mode session setting.
func (c *CustomFuncs) GenericRulesEnabled() bool {
	return c.e.evalCtx.SessionData().PlanCacheMode != sessiondatapb.PlanCacheModeForceCustom
}

// HasPlaceholdersOrStableExprs returns true if the given relational expression's subtree has
// at least one placeholder.
func (c *CustomFuncs) HasPlaceholdersOrStableExprs(e memo.RelExpr) bool {
	return e.Relational().HasPlaceholder || e.Relational().VolatilitySet.HasStable()
}

// GenerateParameterizedJoinValuesAndFilters returns a single-row Values
// expression containing placeholders and stable expressions in the given
// filters. It also returns a new set of filters where the placeholders and
// stable expressions have been replaced with variables referencing the columns
// produced by the returned Values expression. If the given filters have no
// placeholders or stable expressions, ok=false is returned.
func (c *CustomFuncs) GenerateParameterizedJoinValuesAndFilters(
	filters memo.FiltersExpr,
) (values memo.RelExpr, newFilters memo.FiltersExpr, ok bool) {
	var exprs memo.ScalarListExpr
	var cols opt.ColList
	placeholderCols := make(map[tree.PlaceholderIdx]opt.ColumnID)

	// replace recursively walks the expression tree and replaces placeholders
	// and stable expressions. It collects the replaced expressions and creates
	// columns representing those expressions. Those expressions and columns
	// will be used in the Values expression created below.
	var replace func(e opt.Expr) opt.Expr
	replace = func(e opt.Expr) opt.Expr {
		switch t := e.(type) {
		case *memo.PlaceholderExpr:
			idx := t.Value.(*tree.Placeholder).Idx
			// Reuse the same column for duplicate placeholder references.
			if col, ok := placeholderCols[idx]; ok {
				return c.e.f.ConstructVariable(col)
			}
			col := c.e.f.Metadata().AddColumn(fmt.Sprintf("$%d", idx+1), t.DataType())
			placeholderCols[idx] = col
			exprs = append(exprs, t)
			cols = append(cols, col)
			return c.e.f.ConstructVariable(col)

		case *memo.FunctionExpr:
			// TODO(mgartner): Consider including other expressions that could
			// be stable: casts, assignment casts, UDFCallExprs, unary ops,
			// comparisons, binary ops.
			// TODO(mgartner): Include functions with arguments if they are all
			// constants or placeholders.
			if t.Overload.Volatility == volatility.Stable && len(t.Args) == 0 {
				col := c.e.f.Metadata().AddColumn("", t.DataType())
				exprs = append(exprs, t)
				cols = append(cols, col)
				return c.e.f.ConstructVariable(col)
			}
		}

		return c.e.f.Replace(e, replace)
	}

	// Replace placeholders and stable expressions in each filter.
	for i := range filters {
		cond := filters[i].Condition
		if newCond := replace(cond).(opt.ScalarExpr); newCond != cond {
			if newFilters == nil {
				// Lazily allocate newFilters.
				newFilters = make(memo.FiltersExpr, len(filters))
				copy(newFilters, filters[:i])
			}
			// Construct a new filter if placeholders were replaced.
			newFilters[i] = c.e.f.ConstructFiltersItem(newCond)
		} else if newFilters != nil {
			// Otherwise copy the filter if newFilters has been allocated.
			newFilters[i] = filters[i]
		}
	}

	// If no placeholders or stable expressions were replaced, there is nothing
	// to do.
	if len(exprs) == 0 {
		return nil, nil, false
	}

	// Create the Values expression with one row and one column for each
	// replaced expression.
	typs := make([]*types.T, len(exprs))
	for i, e := range exprs {
		typs[i] = e.DataType()
	}
	tupleTyp := types.MakeTuple(typs)
	rows := memo.ScalarListExpr{c.e.f.ConstructTuple(exprs, tupleTyp)}
	values = c.e.f.ConstructValues(rows, &memo.ValuesPrivate{
		Cols: cols,
		ID:   c.e.f.Metadata().NextUniqueID(),
	})

	return values, newFilters, true
}

// ParameterizedJoinPrivate returns JoinPrivate that disabled join reordering and
// merge join exploration.
func (c *CustomFuncs) ParameterizedJoinPrivate() *memo.JoinPrivate {
	return &memo.JoinPrivate{
		Flags:            memo.DisallowMergeJoin,
		SkipReorderJoins: true,
	}
}

// PlaceholderScanSpanAndPrivate returns a span and scan private for a
// PlaceholderScan expression that is semantically equivalent to the given
// lookup join with input values. See
// ConvertParameterizedLookupJoinToPlaceholderScan for more details.
func (c *CustomFuncs) PlaceholderScanSpanAndPrivate(
	lookupPrivate *memo.LookupJoinPrivate,
	values *memo.ValuesExpr,
	row memo.ScalarListExpr,
	outputCols opt.ColSet,
) (span memo.ScalarListExpr, scanPrivate *memo.ScanPrivate, ok bool) {
	// The lookup join must be an inner join.
	if lookupPrivate.JoinType != opt.InnerJoinOp {
		return nil, nil, false
	}
	// The lookup join must only have key columns, no lookup expressions.
	if len(lookupPrivate.KeyCols) == 0 ||
		lookupPrivate.LookupExpr != nil ||
		lookupPrivate.RemoteLookupExpr != nil {
		return nil, nil, false
	}
	// The lookup join must not be part of a paired join.
	if lookupPrivate.IsFirstJoinInPairedJoiner || lookupPrivate.IsSecondJoinInPairedJoiner {
		return nil, nil, false
	}
	// The index must be able to produce all the output columns.
	md := c.e.f.Metadata()
	indexCols := md.TableMeta(lookupPrivate.Table).IndexColumns(lookupPrivate.Index)
	if !outputCols.SubsetOf(indexCols) {
		return nil, nil, false
	}

	// Map columns in the input Values expression to the key columns.
	span = make(memo.ScalarListExpr, len(lookupPrivate.KeyCols))
	for i, keyCol := range lookupPrivate.KeyCols {
		for j, valCol := range values.Cols {
			if keyCol == valCol {
				if !verifyType(md, keyCol, row[j].DataType()) {
					// TODO(mgartner): This was added to copy the same check
					// made while planning the the placeholder fast-path, but it
					// may not be necessary here because the lookup join may
					// have already checked this.
					return nil, nil, false
				}
				span[i] = row[j]
				break
			}
		}
		if span[i] == nil {
			panic(errors.AssertionFailedf("no value found for key column %d", keyCol))
		}
	}

	scanPrivate = &memo.ScanPrivate{
		Table:   lookupPrivate.Table,
		Index:   lookupPrivate.Index,
		Cols:    outputCols.Copy(),
		Locking: lookupPrivate.Locking,
	}
	return span, scanPrivate, true
}
