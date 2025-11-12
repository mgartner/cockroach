// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package rowexec

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/typedesc"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra/execopnode"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra/execreleasable"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/row"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/rowinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/span"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
)

// tableReader is the start of a computation flow; it performs KV operations to
// retrieve rows for a table, runs a filter expression, and passes rows with the
// desired column values to an output RowReceiver.
// See docs/RFCS/distributed_sql.md
type levenshteinReader struct {
	execinfra.ProcessorBase
	execinfra.SpansWithCopy

	col uint32
	// target  string
	maxDist uint64

	sb span.Builder
	m  LevenshteinMatrix

	scanStarted bool
	skipPrefix  string

	// fetcher wraps a row.Fetcher, allowing the tableReader to add a stat
	// collection layer.
	fetcher rowFetcher
	alloc   tree.DatumAlloc

	// rowsRead is the number of rows read and is tracked unconditionally.
	rowsRead     int64
	scansStarted int

	stageID int32
}

var _ execinfra.Processor = &levenshteinReader{}
var _ execinfra.RowSource = &levenshteinReader{}
var _ execreleasable.Releasable = &levenshteinReader{}
var _ execopnode.OpNode = &levenshteinReader{}

const levenshteinReaderProcName = "levenshtein reader"

var lrPool = sync.Pool{
	New: func() interface{} {
		return &levenshteinReader{}
	},
}

// newLevenshteinReader creates a tableReader.
func newLevenshteinReader(
	ctx context.Context,
	flowCtx *execinfra.FlowCtx,
	processorID int32,
	stageID int32,
	spec *execinfrapb.LevenshteinReaderSpec,
	post *execinfrapb.PostProcessSpec,
) (*levenshteinReader, error) {
	// NB: we hit this with a zero NodeID (but !ok) with multi-tenancy.
	if nodeID, ok := flowCtx.NodeID.OptionalNodeID(); ok && nodeID == 0 {
		return nil, errors.AssertionFailedf("attempting to create a levenshtein reader with uninitialized NodeID")
	}

	lr := lrPool.Get().(*levenshteinReader)
	lr.stageID = stageID
	lr.col = spec.Column
	lr.maxDist = spec.MaxDist

	// Make sure the key column types are hydrated. The fetched column types
	// will be hydrated in ProcessorBase.Init below.
	resolver := flowCtx.NewTypeResolver(flowCtx.Txn)
	for i := range spec.FetchSpec.KeyAndSuffixColumns {
		if err := typedesc.EnsureTypeIsHydrated(
			ctx, spec.FetchSpec.KeyAndSuffixColumns[i].Type, &resolver,
		); err != nil {
			return nil, err
		}
	}

	resultTypes := make([]*types.T, len(spec.FetchSpec.FetchedColumns))
	for i := range resultTypes {
		resultTypes[i] = spec.FetchSpec.FetchedColumns[i].Type
	}

	if err := lr.Init(
		ctx,
		lr,
		post,
		resultTypes,
		flowCtx,
		processorID,
		nil, /* memMonitor */
		execinfra.ProcStateOpts{
			// We don't pass lr.input as an inputToDrain; lr.input is just an adapter
			// on top of a Fetcher; draining doesn't apply to it. Moreover, Andrei
			// doesn't trust that the adapter will do the right thing on a Next() call
			// after it had previously returned an error.
			InputsToDrain: nil,
		},
	); err != nil {
		return nil, err
	}

	var fetcher row.Fetcher
	if err := fetcher.Init(
		ctx,
		row.FetcherInitArgs{
			Txn: flowCtx.Txn,
			// Reverse:                    spec.Reverse,
			// LockStrength:               spec.LockingStrength,
			// LockWaitPolicy:             spec.LockingWaitPolicy,
			// LockDurability:             spec.LockingDurability,
			SpansCanOverlap:            false,
			LockTimeout:                flowCtx.EvalCtx.SessionData().LockTimeout,
			DeadlockTimeout:            flowCtx.EvalCtx.SessionData().DeadlockTimeout,
			Alloc:                      &lr.alloc,
			MemMonitor:                 flowCtx.Mon,
			Spec:                       &spec.FetchSpec,
			TraceKV:                    flowCtx.TraceKV,
			ForceProductionKVBatchSize: flowCtx.EvalCtx.TestingKnobs.ForceProductionValues,
		},
	); err != nil {
		return nil, err
	}
	lr.sb.InitWithFetchSpec(flowCtx.EvalCtx, flowCtx.Cfg.Codec, &spec.FetchSpec)
	lr.fetcher = &fetcher
	lr.m.Init(spec.Target)
	return lr, nil
}

// Start is part of the RowSource interface.
func (lr *levenshteinReader) Start(ctx context.Context) {
	if lr.FlowCtx.Txn == nil {
		log.Dev.Fatalf(ctx, "levenshteinReader outside of txn")
	}

	// Keep ctx assignment so we remember StartInternal can make a new one.
	ctx = lr.StartInternal(ctx, levenshteinReaderProcName)
	// Appease the linter.
	_ = ctx
}

const debug = true

func (lr *levenshteinReader) startScan(ctx context.Context) error {
	var values [1]rowenc.EncDatum
	span, _, _, err := lr.sb.SpanFromEncDatumsWithRange(
		ctx,
		values[:],
		0,
		&rowenc.EncDatum{Datum: tree.NewDString("")}, nil,
		true, false,
		types.String,
	)
	if err != nil {
		return err
	}
	spans := roachpb.Spans{span}

	log.VEvent(ctx, 1, "starting scan")
	if debug {
		fmt.Printf("levenshteinReader starting scan at %q\n", span)
	}
	err = lr.fetcher.StartScan(
		ctx, spans, nil /* spanIDs */, rowinfra.GetDefaultBatchBytesLimit(true), rowinfra.NoRowLimit,
	)
	return err
}

func (lr *levenshteinReader) startNextScan(ctx context.Context) error {
	// Note: This mimics logic in sql/opt/idxconstraint for building spans for
	// prefix filters like: col LIKE 'prefix%'.
	startVal := []byte(lr.skipPrefix)
	i := len(startVal) - 1
	for ; i >= 0 && startVal[i] == 0xFF; i-- {
	}
	if i < 0 {
		// If i < 0, we have a prefix like "\xff\xff\xff"; there is no ending value.
		// TODO(mgartner): Will this ever happen?
		return errors.New("levenshteinReader cannot scan with skipPrefix of all 0xff bytes")
	}
	// A few examples:
	//   prefix      -> endValue
	//   ABC         -> ABD
	//   ABC\xff     -> ABD
	//   ABC\xff\xff -> ABD
	startVal = startVal[:i+1]
	startVal[i]++

	var values [1]rowenc.EncDatum
	span, _, _, err := lr.sb.SpanFromEncDatumsWithRange(
		ctx,
		values[:],
		0,
		&rowenc.EncDatum{Datum: tree.NewDString(string(startVal))}, nil,
		true, false,
		types.String,
	)
	if err != nil {
		return err
	}
	spans := roachpb.Spans{span}

	log.VEvent(ctx, 1, "starting scan")
	if debug {
		fmt.Printf("levenshteinReader starting next scan at %q\n", span)
	}
	lr.scansStarted++
	fmt.Printf("scans started: %d\n", lr.scansStarted)
	err = lr.fetcher.RestartScan(
		ctx, spans, nil /* spanIDs */, rowinfra.GetDefaultBatchBytesLimit(true), rowinfra.NoRowLimit,
	)
	return err
}

// Next is part of the RowSource interface.
func (lr *levenshteinReader) Next() (rowenc.EncDatumRow, *execinfrapb.ProducerMetadata) {
	for lr.State == execinfra.StateRunning {
		if !lr.scanStarted {
			if err := lr.startScan(lr.Ctx()); err != nil {
				lr.MoveToDraining(err)
				break
			}
			lr.scanStarted = true
		}

		row, _, err := lr.fetcher.NextRow(lr.Ctx())
		if row == nil || err != nil {
			lr.MoveToDraining(err)
			break
		}

		lr.rowsRead++
		if outRow := lr.ProcessRowHelper(row); outRow != nil {
			val := row[lr.col]
			if err := val.EnsureDecoded(types.String, &lr.alloc); err != nil {
				lr.MoveToDraining(err)
				break
			}
			valStr := string(*val.Datum.(*tree.DString))
			if debug {
				fmt.Printf("scanned value %q\n", valStr)
			}
			// Check if the string column matches.
			isMatch, skipPrefixLen := lr.m.DistanceLTE(valStr, int(lr.maxDist))
			lr.skipPrefix = valStr[:skipPrefixLen]

			// TODO
			if lr.fetcher.EndOfBatch() && lr.skipPrefix != "" {
				if debug {
					fmt.Printf("end of batch with skipPrefix %q\n", lr.skipPrefix)
				}
				if err := lr.startNextScan(lr.Ctx()); err != nil {
					lr.MoveToDraining(err)
					break
				}
			} else if lr.fetcher.EndOfBatch() {
				if debug {
					fmt.Printf("end of batch\n")
				}
			}

			if isMatch {
				return outRow, nil
			}
		}

	}
	return nil, lr.DrainHelper()
}

// Release releases this tableReader back to the pool.
func (lr *levenshteinReader) Release() {
	lr.ProcessorBase.Reset()
	lr.fetcher.Reset()
	// Deeply reset the spans so that we don't hold onto the keys of the spans.
	lr.SpansWithCopy.Reset()
	*lr = levenshteinReader{
		ProcessorBase: lr.ProcessorBase,
		SpansWithCopy: lr.SpansWithCopy,
		fetcher:       lr.fetcher,
		rowsRead:      0,
	}
	lrPool.Put(lr)
}

func (lr *levenshteinReader) close() {
	if lr.InternalClose() {
		if lr.fetcher != nil {
			lr.fetcher.Close(lr.Ctx())
		}
	}
}

// ConsumerClosed is part of the RowSource interface.
func (lr *levenshteinReader) ConsumerClosed() {
	lr.close()
}

// ChildCount is part of the execopnode.OpNode interface.
func (lr *levenshteinReader) ChildCount(bool) int {
	return 0
}

// Child is part of the execopnode.OpNode interface.
func (lr *levenshteinReader) Child(nth int, _ bool) execopnode.OpNode {
	panic(errors.AssertionFailedf("invalid index %d", nth))
}

const (
	initialRows = 4
)

type LevenshteinMatrix struct {
	target   string
	matrix   [][]int
	lastWord string
}

func (m *LevenshteinMatrix) Init(target string) {
	m.target = target
	firstRow := make([]int, len(target)+1)
	for i := range firstRow {
		firstRow[i] = i
	}
	m.matrix = make([][]int, 1, initialRows)
	m.matrix[0] = firstRow
}

func (m *LevenshteinMatrix) DistanceLTE(word string, maxDist int) (_ bool, skipPrefixLen int) {
	// Skip over the common prefix of the word and last word, if there is one.
	// The rows corresponding to this prefix can be reused, avoiding
	// re-calculation.
	pre := 0
	for ; pre < len(word) && pre < len(m.lastWord) && word[pre] == m.lastWord[pre]; pre++ {
	}
	m.matrix = m.matrix[:pre+1]

	// Fill out the matrix for the suffix of rows.
	cols := len(m.target) + 1
	for i := pre; i < len(word); i++ {
		letter := word[i]
		previousRow := m.matrix[len(m.matrix)-1]
		row := m.row(i + 1)
		row[0] = previousRow[0] + 1
		rowMin := row[0]
		for j := 1; j < cols; j++ {
			if letter == m.target[j-1] {
				row[j] = previousRow[j-1]
			} else {
				row[j] = min(row[j-1]+1, previousRow[j]+1, previousRow[j-1]+1)
			}
			rowMin = min(rowMin, row[j])
		}
		if rowMin > maxDist {
			// No need to check the rest of the word.
			// And we can calculate the skipPrefixLen.
			m.lastWord = word[:i+1]
			return false, i + 1
		}
	}
	m.lastWord = word
	return m.matrix[len(m.matrix)-1][cols-1] <= maxDist, 0
}

// row returns the ith row of the matrix. The matrix will grow to fit at least i
// rows if it does not already have capacity.
func (m *LevenshteinMatrix) row(i int) []int {
	if i >= cap(m.matrix) {
		m.matrix = slices.Grow(m.matrix, i-len(m.matrix)+1)
	}
	if i >= len(m.matrix) {
		m.matrix = m.matrix[:i+1]
	}
	if m.matrix[i] == nil {
		m.matrix[i] = make([]int, len(m.target)+1)
	}
	return m.matrix[i]
}
