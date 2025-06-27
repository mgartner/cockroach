// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sql

import (
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catenumpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/colinfo"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/fetchpb"
	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/cat"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/props/physical"
	"github.com/cockroachdb/cockroach/pkg/sql/row"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/rowinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/span"
)

type Program struct {
	ops []op
}

type op func(vm *VM) (ipOffset int)

var opDummy op

func opJump(n int) op {
	return func(vm *VM) int {
		return n
	}
}

func opSetResultCols(md *opt.Metadata, pres physical.Presentation) op {
	args := new(struct {
		cols []colinfo.ResultColumn
	})

	args.cols = make(colinfo.ResultColumns, len(pres))
	for i := range pres {
		args.cols[i] = colinfo.ResultColumn{
			Name: pres[i].Alias,
			Typ:  md.ColumnMeta(pres[i].ID).Type,
		}
	}

	return func(vm *VM) int {
		vm.recv.SetColumns(vm.ctx, args.cols)
		return 1
	}
}

// opAddRow adds the row in the
func opAddRow() op {
	return func(vm *VM) int {
		vm.recv.AddRow(vm.ctx, vm._ROW)
		return 1
	}
}

// opSetTE sets the TE register to the given typed expressions.
func opSetTE(exprs tree.TypedExprs) op {
	return func(vm *VM) int {
		vm._TE = exprs
		return 1
	}
}

// opEvalTE evaluates the expressions in _TE and places the results in _R.
func opEvalTE() op {
	return func(vm *VM) int {
		// Reuse _ROW if it is large enough, otherwise allocate a new one.
		if cap(vm._ROW) < len(vm._TE) {
			vm._ROW = make([]tree.Datum, len(vm._TE))
		}
		vm._ROW = vm._ROW[:len(vm._TE)]
		for i, expr := range vm._TE {
			// Invariant: The expression is either a placeholder or a constant.
			switch t := expr.(type) {
			case *tree.Placeholder:
				val, err := eval.Expr(vm.ctx, vm.evalCtx, t)
				if err != nil {
					panicTODO(err)
				}
				vm._ROW[i] = val
			case tree.Datum:
				vm._ROW[i] = t
			default:
				panicTODO("unexpected expression type")
			}
		}
		return 1
	}
}

// opGenSpan generates a single span from the datums in _ROW and places the span
// in _SP.
func opGenSpan(
	evalCtx *eval.Context, codec keys.SQLCodec, idx cat.Index, tabID descpb.ID, idxID descpb.IndexID,
) op {
	args := new(struct {
		sb     span.Builder
		colMap catalog.TableColMap
	})

	keyCols := make([]fetchpb.IndexFetchSpec_KeyColumn, idx.ColumnCount())
	for i := range keyCols {
		col := idx.Column(i)
		dir := catenumpb.IndexColumn_ASC
		if col.Descending {
			dir = catenumpb.IndexColumn_DESC
		}
		// NOTE: span.Builder.SpanFromDatumRow only uses the ColumnID and
		// direction to build spans, so no other fields need to be set.
		keyCols[i] = fetchpb.IndexFetchSpec_KeyColumn{
			IndexFetchSpec_Column: fetchpb.IndexFetchSpec_Column{
				ColumnID: descpb.ColumnID(col.ColID()),
			},
			Direction: dir,
		}
	}

	for i, n := 0, idx.KeyColumnCount(); i < n; i++ {
		colID := descpb.ColumnID(idx.Column(i).ColID())
		args.colMap.Set(colID, i)
	}

	args.sb.InitAlt(evalCtx, codec, tabID, idxID, keyCols)
	return func(vm *VM) int {
		// Reuse _SP if it can store a single span.
		if cap(vm._SP) < 1 {
			vm._SP = make([]roachpb.Span, 1)
		}
		vm._SP = vm._SP[:1]
		var err error
		vm._SP[0], _, err = args.sb.SpanFromDatumRow(vm._ROW, len(vm._ROW), args.colMap)
		if err != nil {
			panicTODO(err)
		}
		return 1
	}
}

func opFetcherInit(
	codec keys.SQLCodec,
	tab cat.Table,
	tabID opt.TableID,
	cols opt.ColSet,
	tabDesc catalog.TableDescriptor,
	idxDesc catalog.Index,
) op {
	args := new(struct {
		spec fetchpb.IndexFetchSpec
	})

	fetchCols := make([]descpb.ColumnID, 0, cols.Len())
	for col, ok := cols.Next(0); ok; col, ok = cols.Next(col + 1) {
		ord := tabID.ColumnOrdinal(col)
		fetchCols = append(fetchCols, descpb.ColumnID(tab.Column(ord).ColID()))
	}

	// TODO(mgartner): Ideally the fetch spec would not need information from
	// the descriptors to function. We should have all the information we need
	// in the memo.
	err := rowenc.InitIndexFetchSpec(&args.spec, codec, tabDesc, idxDesc, fetchCols)
	if err != nil {
		panicTODO(err)
	}

	return func(vm *VM) int {
		err := vm._F.Init(vm.ctx, row.FetcherInitArgs{
			WillUseKVProvider: false,
			Txn:               vm.evalCtx.Txn,
			Reverse:           false,
			Alloc:             nil,
			Spec:              &args.spec,
			TraceKV:           false,
			SpansCanOverlap:   false,
			// TODO(mgartner): Set the remaining fields, if necessary.
			// MemMonitor:                 nil,
			// TraceKVEvery:               nil,
			// ForceProductionKVBatchSize: flowCtx.EvalCtx.TestingKnobs.ForceProductionValues,
			// LockStrength:               0,
			// LockWaitPolicy:             0,
			// LockDurability:             0,
			// LockTimeout:                flowCtx.EvalCtx.SessionData().LockTimeout,
			// DeadlockTimeout:            flowCtx.EvalCtx.SessionData().DeadlockTimeout,
		})
		if err != nil {
			panicTODO(err)
		}

		// TODO(mgartner): Set the row limit hint.
		var spanIDs []int
		err = vm._F.StartScan(
			vm.ctx, vm._SP, spanIDs, rowinfra.BytesLimit(10000), rowinfra.NoRowLimit,
		)
		if err != nil {
			panicTODO(err)
		}
		return 1
	}
}

func opFetcherNext(jump int) op {
	return func(vm *VM) int {
		var err error
		vm._ROW, _, err = vm._F.NextRowDecoded(vm.ctx)
		if err != nil {
			panicTODO(err)
		}
		if vm._ROW != nil {
			// Jump if there is a row.
			return jump
		}
		// If there is no row, continue to the next instruction.
		return 1
	}
}

func opFetcherClose() op {
	return func(vm *VM) int {
		vm._F.Close(vm.ctx)
		return 1
	}
}
