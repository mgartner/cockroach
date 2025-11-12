// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sql

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/exec"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util"
)

// A scanNode handles scanning over the key/value pairs for a table and
// reconstructing them into rows.
type levenshteinScanNode struct {
	// This struct must be allocated on the heap and its location stay
	// stable after construction because it implements
	// IndexedVarContainer and the IndexedVar objects in sub-expressions
	// will link to it by reference after checkRenderStar / analyzeExpr.
	// Enforce this using NoCopy.
	_ util.NoCopy

	zeroInputPlanNode
	fetchPlanningInfo

	// table      roachpb.TableID
	// index      roachpb.IndexID
	// neededCols exec.TableColumnOrdinalSet
	col     exec.TableColumnOrdinal
	colMap  catalog.TableColMap
	target  string
	maxDist uint64

	// fetcher row.Fetcher
}

// func levenScan(target string, maxDist int) (matches []string) {
// 	var m LevenshteinMatrix
// 	m.Init(target)
// 	skipPrefix := ""
// 	for _, word := range words {
// 		if skipPrefix != "" && strings.HasPrefix(word, skipPrefix) {
// 			fmt.Printf("skipping %q\n", word)
// 			continue
// 		}
// 		match, skipPrefixLen := m.DistanceLTE(word, maxDist)
// 		if match {
// 			matches = append(matches, word)
// 		}
// 		skipPrefix = word[:skipPrefixLen]
// 		fmt.Printf("skip prefix %q\n", skipPrefix)
// 	}
// 	return matches
// }

func (n *levenshteinScanNode) startExec(params runParams) error {
	panic("levenshteinScanNode can't be run in local mode")
	// var spec fetchpb.IndexFetchSpec
	// // fetchCols := make([]descpb.ColumnID, 0, cols.Len())
	// // for col, ok := cols.Next(0); ok; col, ok = cols.Next(col + 1) {
	// // 	ord := tabID.ColumnOrdinal(col)
	// // 	fetchCols = append(fetchCols, descpb.ColumnID(tab.Column(ord).ColID()))
	// // }
	// // TODO(mgartner): Ideally the fetch spec would not need information from
	// // the descriptors to function. We should have all the information we need
	// // in the memo.
	// fetchColIDs := make([]descpb.ColumnID, len(n.columns))
	// for i := range n.catalogCols {
	// 	fetchColIDs[i] = n.catalogCols[i].GetID()
	// }
	// err := rowenc.InitIndexFetchSpec(&spec, params.p.ExecCfg().Codec, n.desc, n.fetchPlanningInfo.index, fetchColIDs)
	// if err != nil {
	// 	return err
	// }
	// n.fetcher.Init(params.ctx, row.FetcherInitArgs{
	// 	// StreamingKVFetcher:         nil,
	// 	// WillUseKVProvider:          false,
	// 	Txn:     params.p.Txn(),
	// 	Reverse: false,
	// 	// LockStrength:               0,
	// 	// LockWaitPolicy:             0,
	// 	// LockDurability:             0,
	// 	// LockTimeout:                0,
	// 	// DeadlockTimeout:            0,
	// 	// Alloc:                      nil,
	// 	// MemMonitor:                 nil,
	// 	Spec: nil,
	// 	// TraceKV:                    false,
	// 	// TraceKVEvery:               nil,
	// 	// ForceProductionKVBatchSize: false,
	// 	SpansCanOverlap: true,
	// })
	//
	// var span constraint.Span
	// span.Init()
	// var c constraint.Constraint
	// c.
	// var spans constraint.Spans
	// var sp constraint.Span
	// key := constraint.MakeKey(tree.DNull)
	// sp.Init(key, constraint.ExcludeBoundary, constraint.EmptyKey, constraint.ExcludeBoundary)
	// for _, child := range els {
	// 	datum := ExtractConstDatum(child)
	// 	if !cb.verifyType(col, datum.ResolvedType()) {
	// 		return unconstrained, false
	// 	}
	// 	if datum == tree.DNull {
	// 		// Ignore NULLs - they can't match any values
	// 		continue
	// 	}
	// 	key := constraint.MakeKey(datum)
	// 	sp.Init(key, includeBoundary, key, includeBoundary)
	// 	spans.Append(&sp)
	// }
	// var c constraint.Constraint
	// spans.SortAndMerge(&keyCtx)
	// c.Init(&keyCtx, &spans)
	// return constraint.SingleConstraint(&c), true
	//
	// var sb span.Builder
	// sb.Init(params.EvalContext(), params.p.ExecCfg().Codec, n.desc, n.fetchPlanningInfo.index)
	// sb.SpanFromDatumRow([]tree.Datum{tree.DNull}, 1, n.colMap)
	//
	// n.fetcher.StartScan()
	// return nil
}

func (n *levenshteinScanNode) Close(context.Context) {}

func (n *levenshteinScanNode) Next(params runParams) (bool, error) {
	panic("levenshteinScanNode can't be run in local mode")
}

func (n *levenshteinScanNode) Values() tree.Datums {
	panic("scanNode can't be run in local mode")
}

// const (
// 	initialRows = 4
// )
//
// type LevenshteinMatrix struct {
// 	target   string
// 	matrix   [][]int
// 	lastWord string
// }
//
// func (m *LevenshteinMatrix) Init(target string) {
// 	m.target = target
// 	firstRow := make([]int, len(target)+1)
// 	for i := range firstRow {
// 		firstRow[i] = i
// 	}
// 	m.matrix = make([][]int, 1, initialRows)
// 	m.matrix[0] = firstRow
// }
//
// func (m *LevenshteinMatrix) DistanceLTE(word string, maxDist int) (_ bool, skipPrefixLen int) {
// 	// Skip over the common prefix of the word and last word, if there is one.
// 	// The rows corresponding to this prefix can be reused, avoiding
// 	// re-calculation.
// 	pre := 0
// 	for ; pre < len(word) && pre < len(m.lastWord) && word[pre] == m.lastWord[pre]; pre++ {
// 	}
// 	m.matrix = m.matrix[:pre+1]
//
// 	// Fill out the matrix for the suffix of rows.
// 	cols := len(m.target) + 1
// 	for i := pre; i < len(word); i++ {
// 		letter := word[i]
// 		previousRow := m.matrix[len(m.matrix)-1]
// 		row := m.row(i + 1)
// 		row[0] = previousRow[0] + 1
// 		rowMin := row[0]
// 		for j := 1; j < cols; j++ {
// 			if letter == m.target[j-1] {
// 				row[j] = previousRow[j-1]
// 			} else {
// 				row[j] = min(row[j-1]+1, previousRow[j]+1, previousRow[j-1]+1)
// 			}
// 			rowMin = min(rowMin, row[j])
// 		}
// 		if rowMin > maxDist {
// 			// No need to check the rest of the word.
// 			// And we can calculate the skipPrefixLen.
// 			m.lastWord = word[:i+1]
// 			return false, i + 1
// 		}
// 	}
// 	m.lastWord = word
// 	return m.matrix[len(m.matrix)-1][cols-1] <= maxDist, 0
// }
//
// // row returns the ith row of the matrix. The matrix will grow to fit at least i
// // rows if it does not already have capacity.
// func (m *LevenshteinMatrix) row(i int) []int {
// 	if i >= cap(m.matrix) {
// 		m.matrix = slices.Grow(m.matrix, i-len(m.matrix)+1)
// 	}
// 	if i >= len(m.matrix) {
// 		m.matrix = m.matrix[:i+1]
// 	}
// 	if m.matrix[i] == nil {
// 		m.matrix[i] = make([]int, len(m.target)+1)
// 	}
// 	return m.matrix[i]
// }
