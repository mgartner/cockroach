// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sql

import (
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
)

type Compiler struct {
	mem     *memo.Memo
	md      *opt.Metadata
	evalCtx *eval.Context
	codec   keys.SQLCodec

	ops []op

	// Saved instruction pointers for jumping.
	doneIP int
	nextIP int
}

func (c *Compiler) Init(evalCtx *eval.Context, mem *memo.Memo, codec keys.SQLCodec) {
	*c = Compiler{
		mem:     mem,
		md:      mem.Metadata(),
		evalCtx: evalCtx,
		codec:   codec,
	}
}

func (c *Compiler) push(o op) {
	c.ops = append(c.ops, o)
}

func (c *Compiler) swap(ip int, o op) {
	c.ops[ip] = o
}

func (c *Compiler) Compile() (_ Program, ok bool) {
	// Compile the prologue and jump over the epilogue.
	pres := c.mem.RootExpr().(memo.RelExpr).RequiredPhysical().Presentation
	c.push(opSetResultCols(c.md, pres))
	c.push(opJump(2))

	// Compile the epilogue. It is just a dummy op that will be replaced later
	// on by a jump to the end of the program, which will halt the VM.
	c.doneIP = len(c.ops)
	c.push(opDummy)

	// Compile the expression.
	if ok := c.compileExpr(c.mem.RootExpr()); !ok {
		return Program{}, false
	}

	// Compile row emission and jumping back to begin processing the next row.
	c.push(opAddRow())
	c.push(opJump(c.nextIP - len(c.ops)))

	// Patch the dummy op in the epilogue with a jump to the end of the program.
	c.swap(c.doneIP, opJump(len(c.ops)-c.doneIP))

	return Program{ops: c.ops}, true
}

func (c *Compiler) compileExpr(e opt.Expr) (ok bool) {
	switch t := e.(type) {
	case *memo.PlaceholderScanExpr:
		return c.compilePlaceholderScan(t)
	default:
		return false
	}
}

func (c *Compiler) compilePlaceholderScan(e *memo.PlaceholderScanExpr) bool {
	tab := c.md.Table(e.Table)
	idx := tab.Index(e.Index)
	tabID := descpb.ID(tab.ID())
	idxID := descpb.IndexID(idx.ID())

	// TODO(mgartner): Ideally the kv fetcher would not need information from
	// the descriptors. This is preventing the VM, compiler, and ops from having
	// their own package. We can currently only access the descriptors through
	// optTable and optIndex. We should have all the information we need in the
	// memo.
	tabDesc := tab.(*optTable).desc
	idxDesc := idx.(*optIndex).idx

	// First, evaluate the typed expressions to replace placeholders with
	// datums.
	// and generate a span to scan.setup the spans by evaluating the expressions and ge
	exprs, ok := constantsAndPlaceholders(e.Span)
	if !ok {
		return false
	}
	c.push(opSetTE(exprs))
	c.push(opEvalTE())

	// Next, generate a span and start the scan.
	c.push(opGenSpan(c.evalCtx, c.codec, idx, tabID, idxID))
	c.push(opFetcherInit(c.codec, tab, e.Table, e.Cols, tabDesc, idxDesc))

	// Fetch the next row. If a row is fetched, jump over the following
	// instructions to close the fetcher and jump to the epilogue.
	// Save the instruction pointer for looping to the next row.
	c.nextIP = len(c.ops)
	c.push(opFetcherNext(3)) // jump over the close and jump op if a row is fetched

	// If no row is fetch, close the fetcher and jump to the epilogue.
	c.push(opFetcherClose())
	c.push(opJump(c.doneIP - len(c.ops))) // jump to the epilogue

	return true
}

func constantsAndPlaceholders(expr memo.ScalarListExpr) (_ []tree.TypedExpr, ok bool) {
	// This function returns a slice of TypedExprs that are either constants or
	// placeholders. It is used to extract the values from the Span of a
	// PlaceholderScanExpr.
	exprs := make([]tree.TypedExpr, len(expr))
	for i, e := range expr {
		// The expression is either a placeholder or a constant.
		if opt.IsConstValueOp(e) {
			exprs[i] = memo.ExtractConstDatum(e)
			continue
		}
		if p, ok := e.(*memo.PlaceholderExpr); ok {
			exprs[i] = p.Value
			continue
		}
		return nil, false
	}
	return exprs, true
}
