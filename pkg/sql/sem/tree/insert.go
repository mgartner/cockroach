// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in licenses/BSD-vitess.txt.

// Portions of this file are additionally subject to the following
// license and copyright.
//
// Copyright 2015 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// This code was derived from https://github.com/youtube/vitess.

package tree

// Insert represents an INSERT statement.
type Insert struct {
	With       *With
	Table      TableExpr
	Columns    NameList
	Rows       *Select
	OnConflict *OnConflict
	Returning  ReturningClause
}

// Format implements the NodeFormatter interface.
func (node *Insert) Format(ctx *FmtCtx) {
	ctx.FormatNode(node.With)
	if node.OnConflict.IsUpsertAlias() {
		ctx.WriteString("UPSERT")
	} else {
		ctx.WriteString("INSERT")
	}
	ctx.WriteString(" INTO ")
	ctx.FormatNode(node.Table)
	if node.Columns != nil {
		ctx.WriteByte('(')
		ctx.FormatNode(&node.Columns)
		ctx.WriteByte(')')
	}
	if node.DefaultValues() {
		ctx.WriteString(" DEFAULT VALUES")
	} else {
		ctx.WriteByte(' ')
		ctx.FormatNode(node.Rows)
	}
	if node.OnConflict != nil && !node.OnConflict.IsUpsertAlias() {
		ctx.WriteString(" ON CONFLICT")
		if node.OnConflict.Constraint != "" {
			ctx.WriteString(" ON CONSTRAINT ")
			ctx.FormatNode(&node.OnConflict.Constraint)
		}
		if len(node.OnConflict.Columns) > 0 {
			ctx.WriteString(" (")
			ctx.FormatNode(&node.OnConflict.Columns)
			ctx.WriteString(")")
		}
		if node.OnConflict.ArbiterPredicate != nil {
			ctx.WriteString(" WHERE ")
			ctx.FormatNode(node.OnConflict.ArbiterPredicate)
		}
		if node.OnConflict.DoNothing {
			ctx.WriteString(" DO NOTHING")
		} else {
			ctx.WriteString(" DO UPDATE SET ")
			ctx.FormatNode(&node.OnConflict.Exprs)
			if node.OnConflict.Where != nil {
				ctx.WriteByte(' ')
				ctx.FormatNode(node.OnConflict.Where)
			}
		}
	}
	if HasReturningClause(node.Returning) {
		ctx.WriteByte(' ')
		ctx.FormatNode(node.Returning)
	}
}

// DefaultValues returns true iff only default values are being inserted.
func (node *Insert) DefaultValues() bool {
	return node.Rows.Select == nil
}

// VectorInsert returns true iff the input for the insert operation is a
// materialized columnar batch, in which case the Insert operator will use a
// vectorized implementation. This should only be true for the Copy fast path.
func (node *Insert) VectorInsert() bool {
	literalValues, ok := node.Rows.Select.(*LiteralValuesClause)
	if !ok {
		return false
	}
	_, vectorRows := literalValues.Rows.(*VectorRows)
	return vectorRows
}

// OnConflict represents an `ON CONFLICT (columns) WHERE arbiter DO UPDATE SET
// exprs WHERE where` clause.
//
// The zero value for OnConflict is used to signal the UPSERT short form, which
// uses the primary key for as the conflict index and the values being inserted
// for Exprs.
type OnConflict struct {
	// At most one of Columns and Constraint will be set at once.
	// Columns is the list of arbiter columns, if set, that the user specified
	// in the ON CONFLICT (columns) list.
	Columns NameList
	// Constraint is the name of a table constraint that the user specified to
	// get the list of arbiter columns from, in the ON CONFLICT ON CONSTRAINT
	// form.
	Constraint       Name
	ArbiterPredicate Expr
	Exprs            UpdateExprs
	Where            *Where
	DoNothing        bool
}

// IsUpsertAlias returns true if the UPSERT syntactic sugar was used.
func (oc *OnConflict) IsUpsertAlias() bool {
	return oc != nil && oc.Columns == nil && oc.Constraint == "" &&
		oc.ArbiterPredicate == nil && oc.Exprs == nil && oc.Where == nil && !oc.DoNothing
}
