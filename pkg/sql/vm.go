// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sql

import (
	"context"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/colinfo"
	"github.com/cockroachdb/cockroach/pkg/sql/row"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
)

// The Compiler only need to handle a memo where the root expresion is a
// PlaceholderScanExpr, for this prototype.
//
// Things we need to do for a placeholder scan:
//   1. Encode the span ScalarListExpr into key span.
//   2. Start the KV scan (maybe using kvfetcher).
//   3. For each resulting KV
//
// Here's an example of an entire program for the query:
//
//  SELECT * FROM t WHERE k = $1 AND v = $2;
//
// This query's plan has the form (Select (PlaceholderScan)).
//
// -- Prologue
// 0  set result columns
// 1  jump +2
//
// -- Epilogue
// 2  jump +10 (halt)
//
// -- PlaceholderScan
// 3  set typed exprs
// 4  eval typed exprs
// 5  generate key
// 6  init kv fetcher
// 7  next kv fetcher, jump +2, set nextIP to 8
// 8  close kv fetcher
// 9  jump -7
//
// -- Root Expr
// 12 add result row
// 13 goto 8
//

// Benefits of this VM design:
//
// - The separation of compile-time logic (logic that only occurs once per
// PREPARE) and run-time logic (logic that occurs ones per EXECUTE) is clear.
// The former is not in the op closures, the latter is.
//
// - Conditional logic for special cases can be avoided completed in non-special
// cases. We even avoid the overhead of conditions because those ops won't be
// compiled.
//
// - Working memory, i.e., ephemeral memory that is needed to execute the query
// is clear. It is contained within the VM struct, and it can be
// reused across executions of different queries.
//
// - The stack should be flatter, making it easier to reason identify
// performance critical operators and where the program is spending time.
//
// - The VM makes it harder to call arbitrary Go code. This constraint will
// help maintain performance over time as the capabilites of the VM grow.
//
// - So far debugging the VM has been really easy. Simply setting a breakpoint
// on the Execute loop and stepping through ops makes it easy to find certain
// types of bugs, like incorrectly ordered ops, incorrect jump offsets, etc.
//
//
// AI:
// - The VM is simple and easy to understand. It is a simple loop that executes
// a list of operations, each of which is a closure that takes the VM as an
// argument. The operations are defined in the Compiler, and they are executed
// in the order they are defined. This makes it easy to reason about the
// execution flow of the query. The VM is not a stack machine, so it does not
// have the overhead of pushing and popping values from a stack. Instead, it
// uses registers to hold values that are needed for the execution of the query.
//
// - The VM is not a bytecode interpreter, so it does not have the overhead of
// parsing and executing bytecode. Instead, it uses closures that are defined
// at compile time. This makes it easy to reason about the execution flow of
// the query, and it allows for optimizations that are not possible with a
// bytecode interpreter. The closures are defined in the Compiler, and they

type VM struct {
	ctx     context.Context
	evalCtx *eval.Context
	recv    RowReceiver

	// The fields below, prefixed with "_", are "registers" that can be used to
	// store and access values across multiple ops during execution of a program
	// VM.
	_ROW []tree.Datum
	_TE  []tree.TypedExpr
	_SP  []roachpb.Span
	_F   row.Fetcher
}

type RowReceiver interface {
	AddRow(ctx context.Context, row tree.Datums) error
	SetColumns(context.Context, colinfo.ResultColumns)
}

func (vm *VM) Init(ctx context.Context, evalCtx *eval.Context, recv RowReceiver) {
	// TODO(mgartner): Reuse the registers if they are not empty.
	*vm = VM{
		ctx:     ctx,
		evalCtx: evalCtx,
		recv:    recv,
	}
}

// TODO(mgartner): p doesn't need to be a pointer.
func (vm *VM) Execute(p *Program) {
	ip := 0
	for ip < len(p.ops) {
		ip += p.ops[ip](vm)
	}
}

// TODO(mgartner): Handle unexpected errors.
func panicTODO(err any) {
	panic(fmt.Sprintf("unexpected error: %s", err))
}
