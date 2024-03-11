// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package constraint

type PConstraint struct {
	c Constraint
}

// Init initializes the constraint to the columns in the key context and with
// the given spans.
func (p *PConstraint) Init(keyCtx *KeyContext, spans *Spans) {
	p.c = Constraint{
		Columns: keyCtx.Columns,
		Spans:   *spans,
	}
	p.c.Spans.makeImmutable()
}

func (p *PConstraint) Spans() *Spans {
	return &p.c.Spans
}

// InitSingleSpan initializes the constraint to the columns in the key context
// and with one span.
// func (p *PConstraint) InitSingleSpan(keyCtx *KeyContext, span *Span) {
// 	p.c.InitSingleSpan(keyCtx, span)
// }

// TODO
// func (p *PConstraint) AllocSpans(capacity int) {
// 	p.c.Spans.Alloc(capacity)
// }

// AddSpan adds the given span to the constraint. It must not overlap any other
// span.
// func (p *PConstraint) AddSpan(keyCtx *KeyContext, span *Span) {
// 	// TODO: Alloc ahead of time.
// 	// TODO: Error if there is detectable overlap.
// 	p.c.Spans.Append(span)
// }
