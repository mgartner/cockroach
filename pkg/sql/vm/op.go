// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package vm

import (
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
)

type Op struct {
	OpCode
	Table, Index uint32
	Dir          encoding.Direction
	ColID        descpb.ColumnID
	Typ          *types.T
	Codec        keys.SQLCodec
	Expr         tree.TypedExpr
}

type OpCode uint8

const (
	_ OpCode = iota + 1
	ExitIfValEmpty
	Get
	KEncExpr
	KEncPrefix
	VDec
	Push
)
