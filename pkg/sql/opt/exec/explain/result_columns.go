// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package explain

import (
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/sql/catalog/colinfo"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/cat"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/exec"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/errorutil"
	"github.com/cockroachdb/errors"
)

// getResultColumns calculates the result columns of an exec.Node, given the
// arguments passed to the Construct function and the ResultColumns of all its
// input exec.Nodes.
func getResultColumns(
	op execOperator, args interface{}, inputs ...colinfo.ResultColumns,
) (out colinfo.ResultColumns, err error) {
	defer func() {
		if r := recover(); r != nil {
			// If we have a bug in the code below, it's easily possible to hit panic
			// (like out-of-bounds). Catch these here and return as an error.
			if ok, e := errorutil.ShouldCatch(r); ok {
				err = e
			} else {
				panic(r)
			}
		}
	}()

	switch op {
	case filterOp, invertedFilterOp, limitOp, max1RowOp, sortOp, topKOp, bufferOp, hashSetOpOp,
		streamingSetOpOp, unionAllOp, distinctOp, saveTableOp, recursiveCTEOp:
		// These ops inherit the columns from their first input.
		if len(inputs) == 0 {
			return nil, nil
		}
		return inputs[0], nil

	case simpleProjectOp:
		if len(inputs) == 0 {
			return nil, nil
		}
		a := args.(*simpleProjectArgs)
		return projectCols(inputs[0], a.Cols, nil /* colNames */), nil

	case serializingProjectOp:
		if len(inputs) == 0 {
			return nil, nil
		}
		a := args.(*serializingProjectArgs)
		return projectCols(inputs[0], a.Cols, a.ColNames), nil

	case scanOp:
		a := args.(*scanArgs)
		return tableColumns(a.Table, a.Params.NeededCols), nil

	case indexJoinOp:
		a := args.(*indexJoinArgs)
		return tableColumns(a.Table, a.TableCols), nil

	case valuesOp:
		return args.(*valuesArgs).Columns, nil

	case renderOp:
		return args.(*renderArgs).Columns, nil

	case projectSetOp:
		if len(inputs) == 0 {
			return nil, nil
		}
		return appendColumns(inputs[0], args.(*projectSetArgs).ZipCols...), nil

	case applyJoinOp:
		if len(inputs) == 0 {
			return nil, nil
		}
		a := args.(*applyJoinArgs)
		return joinColumns(a.JoinType, inputs[0], a.RightColumns), nil

	case hashJoinOp:
		if len(inputs) < 2 {
			return nil, nil
		}
		return joinColumns(args.(*hashJoinArgs).JoinType, inputs[0], inputs[1]), nil

	case mergeJoinOp:
		if len(inputs) < 2 {
			return nil, nil
		}
		return joinColumns(args.(*mergeJoinArgs).JoinType, inputs[0], inputs[1]), nil

	case lookupJoinOp:
		if len(inputs) == 0 {
			return nil, nil
		}
		a := args.(*lookupJoinArgs)
		cols := joinColumns(a.JoinType, inputs[0], tableColumns(a.Table, a.LookupCols))
		// The following matches the behavior of execFactory.ConstructLookupJoin.
		if a.IsFirstJoinInPairedJoiner {
			cols = append(cols, colinfo.ResultColumn{Name: "cont", Typ: types.Bool})
		}
		return cols, nil

	case ordinalityOp:
		if len(inputs) == 0 {
			return nil, nil
		}
		return appendColumns(inputs[0], colinfo.ResultColumn{
			Name: args.(*ordinalityArgs).ColName,
			Typ:  types.Int,
		}), nil

	case groupByOp:
		if len(inputs) == 0 {
			return nil, nil
		}
		a := args.(*groupByArgs)
		return groupByColumns(inputs[0], a.GroupCols, a.Aggregations), nil

	case scalarGroupByOp:
		if len(inputs) == 0 {
			return nil, nil
		}
		a := args.(*scalarGroupByArgs)
		return groupByColumns(inputs[0], nil /* groupCols */, a.Aggregations), nil

	case windowOp:
		return args.(*windowArgs).Window.Cols, nil

	case invertedJoinOp:
		if len(inputs) == 0 {
			return nil, nil
		}
		a := args.(*invertedJoinArgs)
		cols := joinColumns(a.JoinType, inputs[0], tableColumns(a.Table, a.LookupCols))
		// The following matches the behavior of execFactory.ConstructInvertedJoin.
		if a.IsFirstJoinInPairedJoiner {
			cols = append(cols, colinfo.ResultColumn{Name: "cont", Typ: types.Bool})
		}
		return cols, nil

	case zigzagJoinOp:
		a := args.(*zigzagJoinArgs)
		return appendColumns(
			tableColumns(a.LeftTable, a.LeftCols),
			tableColumns(a.RightTable, a.RightCols)...,
		), nil

	case scanBufferOp:
		a := args.(*scanBufferArgs)
		// TODO: instead of nil check can we put in a fake value?
		if a.Ref == nil {
			return nil, nil
		}
		return a.Ref.Columns(), nil

	case vectorSearchOp:
		a := args.(*vectorSearchArgs)
		return tableColumns(a.Table, a.OutCols), nil

	case vectorMutationSearchOp:
		a := args.(*vectorMutationSearchArgs)
		cols := appendColumns(inputs[0], colinfo.ResultColumn{Name: "partition-key", Typ: types.Int})
		if a.IsIndexPut {
			cols = append(cols, colinfo.ResultColumn{Name: "quantized-vector", Typ: types.Bytes})
		}
		return cols, nil

	case insertOp:
		a := args.(*insertArgs)
		return tableColumns(a.Table, a.ReturnCols), nil

	case insertFastPathOp:
		a := args.(*insertFastPathArgs)
		return tableColumns(a.Table, a.ReturnCols), nil

	case updateOp:
		a := args.(*updateArgs)
		return appendColumns(
			tableColumns(a.Table, a.ReturnCols),
			a.Passthrough...,
		), nil

	case updateSwapOp:
		a := args.(*updateSwapArgs)
		return appendColumns(
			tableColumns(a.Table, a.ReturnCols),
			a.Passthrough...,
		), nil

	case upsertOp:
		a := args.(*upsertArgs)
		return tableColumns(a.Table, a.ReturnCols), nil

	case deleteOp:
		a := args.(*deleteArgs)
		return appendColumns(
			tableColumns(a.Table, a.ReturnCols),
			a.Passthrough...,
		), nil

	case deleteSwapOp:
		a := args.(*deleteSwapArgs)
		return appendColumns(
			tableColumns(a.Table, a.ReturnCols),
			a.Passthrough...,
		), nil

	case opaqueOp:
		if args.(*opaqueArgs).Metadata != nil {
			return args.(*opaqueArgs).Metadata.Columns(), nil
		}
		return nil, nil

	case showCompletionsOp:
		return colinfo.ShowCompletionsColumns, nil

	case alterTableSplitOp:
		return colinfo.AlterTableSplitColumns, nil

	case alterTableUnsplitOp, alterTableUnsplitAllOp:
		return colinfo.AlterTableUnsplitColumns, nil

	case alterTableRelocateOp:
		return colinfo.AlterTableRelocateColumns, nil

	case alterRangeRelocateOp:
		return colinfo.AlterRangeRelocateColumns, nil

	case exportOp:
		return colinfo.ExportColumns, nil

	case sequenceSelectOp:
		return colinfo.SequenceSelectColumns, nil

	case explainOp:
		return colinfo.ExplainPlanColumns, nil

	case explainOptOp:
		return colinfo.ExplainPlanColumns, nil

	case showTraceOp:
		if args.(*showTraceArgs).Compact {
			return colinfo.ShowCompactTraceColumns, nil
		}
		return colinfo.ShowTraceColumns, nil

	case createTableOp, createTableAsOp, createViewOp, controlJobsOp, controlSchedulesOp,
		cancelQueriesOp, cancelSessionsOp, createStatisticsOp, errorIfRowsOp, deleteRangeOp,
		createFunctionOp, createTriggerOp, callOp:
		// These operations produce no columns.
		return nil, nil

	default:
		return nil, errors.AssertionFailedf("unhandled op %d", op)
	}
}

func tableColumns(table cat.Table, ordinals exec.TableColumnOrdinalSet) colinfo.ResultColumns {
	if table == nil {
		return nil
	}
	cols := make(colinfo.ResultColumns, 0, ordinals.Len())
	for i, ok := ordinals.Next(0); ok; i, ok = ordinals.Next(i + 1) {
		// Be defensive about bitset values because they may come from cached
		// gists and the columns they refer to could have been removed.
		if i < table.ColumnCount() {
			col := table.Column(i)
			cols = append(cols, colinfo.ResultColumn{
				Name: string(col.ColName()),
				Typ:  col.DatumType(),
			})
		} else {
			// Give downstream operators something to chew on so that they don't panic.
			cols = append(cols, colinfo.ResultColumn{
				Name: fmt.Sprintf("unknownCol-%d", i),
				Typ:  types.Unknown,
			})
		}
	}
	return cols
}

func joinColumns(
	joinType descpb.JoinType, left, right colinfo.ResultColumns,
) colinfo.ResultColumns {
	if !joinType.ShouldIncludeLeftColsInOutput() {
		return right
	}
	if !joinType.ShouldIncludeRightColsInOutput() {
		return left
	}
	return appendColumns(left, right...)
}

func projectCols(
	input colinfo.ResultColumns, ordinals []exec.NodeColumnOrdinal, colNames []string,
) colinfo.ResultColumns {
	columns := make(colinfo.ResultColumns, len(ordinals))
	for i, ord := range ordinals {
		if int(ord) >= len(input) {
			continue
		}
		columns[i] = input[ord]
		if colNames != nil {
			columns[i].Name = colNames[i]
		}
	}
	return columns
}

func groupByColumns(
	inputCols colinfo.ResultColumns, groupCols []exec.NodeColumnOrdinal, aggregations []exec.AggInfo,
) colinfo.ResultColumns {
	columns := make(colinfo.ResultColumns, 0, len(groupCols)+len(aggregations))
	if inputCols != nil {
		for _, col := range groupCols {
			if len(inputCols) > int(col) {
				columns = append(columns, inputCols[col])
			}
		}
	}
	for _, agg := range aggregations {
		columns = append(columns, colinfo.ResultColumn{
			Name: agg.FuncName,
			Typ:  agg.ResultType,
		})
	}
	return columns
}

func appendColumns(
	input colinfo.ResultColumns, others ...colinfo.ResultColumn,
) colinfo.ResultColumns {
	res := make(colinfo.ResultColumns, len(input)+len(others))
	copy(res, input)
	copy(res[len(input):], others)
	return res
}
