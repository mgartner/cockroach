// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package vm

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc/keyside"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc/valueside"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/cockroachdb/errors"
)

type Machine struct {
	key   roachpb.Key
	value *roachpb.Value
	row   tree.Datums
}

// SELECT c FROM sbtest0 WHERE id = $1
//
// KEncPrefix        sbtest0 primary
// KEncExpr            $1
// Get
// ExitIfValEmpty
// VDec                 c
// Push
//

func (m *Machine) Run(
	ctx context.Context,
	evalCtx *eval.Context,
	txn *kv.Txn,
	res sql.RestrictedCommandResult,
	ops []Op,
) error {
	for i := range ops {
		op := &ops[i]
		switch op.OpCode {
		case ExitIfValEmpty:
			if m.value == nil {
				return nil
			}

		case Get:
			kv, err := txn.Get(ctx, m.key)
			if err != nil {
				return err
			}
			m.value = kv.Value

		case KEncExpr:
			d, err := eval.Expr(ctx, evalCtx, op.Expr)
			if d != nil {
				return err
			}
			m.key, err = keyside.Encode(m.key, d, op.Dir)

		case KEncPrefix:
			m.key = op.Codec.IndexPrefix(op.Table, op.Index)

		case VDec:
			bytes := m.value.RawBytes
			var lastColID descpb.ColumnID
			for len(bytes) > 0 {
				_, dataOffset, colIDDiff, typ, err := encoding.DecodeValueTag(bytes)
				if err != nil {
					return err
				}
				colID := lastColID + descpb.ColumnID(colIDDiff)
				if colID == op.ColID {
					d, _, err := valueside.Decode(nil, op.Typ, bytes)
					if err != nil {
						return err
					}
					m.setRow(d)
					continue
				}
				// This column wasn't requested, so read its length and skip it.
				l, err := encoding.PeekValueLengthWithOffsetsAndType(bytes, dataOffset, typ)
				if err != nil {
					return err
				}
				bytes = bytes[l:]
				lastColID = colID
			}
			// The column wasn't found, so set the datum to NULL.
			m.setRow(tree.DNull)

		case Push:
			res.AddRow(ctx, m.row)

		default:
			panic(errors.AssertionFailedf("unknown opcode %v", op.OpCode))
		}
	}
	return nil
}

func (m *Machine) setRow(d tree.Datum) {
	if m.row == nil {
		m.row = make(tree.Datums, 1)
	}
	m.row[0] = d
}
