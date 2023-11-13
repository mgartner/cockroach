// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package constraint

import "github.com/cockroachdb/cockroach/pkg/sql/sem/tree"

type SuffixSpan struct {
	// Start is the starting boundary for the span.
	start SuffixKey

	// End is the ending boundary for the span.
	end SuffixKey

	// startBoundary indicates whether the span contains the start key value.
	startBoundary SpanBoundary

	// endBoundary indicates whether the span contains the end key value.
	endBoundary SpanBoundary
}

// MakeSuffixSpan sets the boundaries of this span to the given values. The following
// spans are not allowed:
// TODO
//  1. Empty span (should never be used in a constraint); not verified.
//  2. Exclusive empty key boundary (use inclusive instead); causes panic.
func MakeSuffixSpan(
	start SuffixKey, startBoundary SpanBoundary, end SuffixKey, endBoundary SpanBoundary,
) SuffixSpan {
	return SuffixSpan{
		start:         start,
		startBoundary: startBoundary,
		end:           end,
		endBoundary:   endBoundary,
	}
}

// StartKey returns the start key.
func (sp *SuffixSpan) StartKey() SuffixKey {
	return sp.start
}

// StartBoundary returns whether the start key is included or excluded.
func (sp *SuffixSpan) StartBoundary() SpanBoundary {
	return sp.startBoundary
}

// EndKey returns the end key.
func (sp *SuffixSpan) EndKey() SuffixKey {
	return sp.end
}

// EndBoundary returns whether the end key is included or excluded.
func (sp *SuffixSpan) EndBoundary() SpanBoundary {
	return sp.endBoundary
}

func (sp *SuffixSpan) Compare(keyCtx *KeyContext, other SuffixSpan) int {
	// Span with lowest start boundary is less than the other.
	if cmp := sp.CompareStarts(keyCtx, other); cmp != 0 {
		return cmp
	}

	// Start boundary is same, so span with lowest end boundary is less than
	// the other.
	if cmp := sp.CompareEnds(keyCtx, other); cmp != 0 {
		return cmp
	}

	// End boundary is same as well, so spans are the same.
	return 0
}

func (sp *SuffixSpan) CompareStarts(keyCtx *KeyContext, other SuffixSpan) int {
	return sp.start.Compare(keyCtx, other.start, sp.startExt(), other.startExt())
}

func (sp *SuffixSpan) CompareStartsWithSpan(keyCtx *KeyContext, other *Span) int {
	return sp.start.CompareToKey(keyCtx, other.start, sp.startExt(), other.startExt())
}

func (sp *SuffixSpan) CompareEnds(keyCtx *KeyContext, other SuffixSpan) int {
	return sp.end.Compare(keyCtx, other.end, sp.endExt(), other.endExt())
}

func (sp *SuffixSpan) CompareEndsWithSpan(keyCtx *KeyContext, other *Span) int {
	return sp.end.CompareToKey(keyCtx, other.end, sp.endExt(), other.endExt())
}

func (sp *SuffixSpan) StartsAfterSpan(keyCtx *KeyContext, other *Span) bool {
	return sp.start.CompareToKey(keyCtx, other.end, sp.startExt(), other.endExt()) >= 0
}

// TryIntersectWithSpan finds the overlap between this span and the given span.
// This span is updated to only cover the range that is common to both spans. If
// there is no overlap, then this span will not be updated, and
// TryIntersectWithSpan will return intersects=false. If the span is not updated,
// TryIntersectWithSpan will return mutated=false.
func (sp *SuffixSpan) TryIntersectWithSpan(
	keyCtx *KeyContext, other *Span,
) (intersects, mutated bool) {
	cmpStarts := sp.CompareStartsWithSpan(keyCtx, other)
	if cmpStarts > 0 {
		// If this span's start boundary is >= the other span's end boundary,
		// then intersection is empty.
		if sp.start.CompareToKey(keyCtx, other.end, sp.startExt(), other.endExt()) >= 0 {
			return false, false
		}
	}

	cmpEnds := sp.CompareEndsWithSpan(keyCtx, other)
	if cmpEnds < 0 {
		// If this span's end boundary is <= the other span's start boundary,
		// then intersection is empty.
		if sp.end.CompareToKey(keyCtx, other.start, sp.endExt(), other.startExt()) <= 0 {
			return false, false
		}
	}

	// Only update now that it's known that intersection is not empty.
	idx := sp.start.index
	if cmpStarts < 0 {
		sp.start = MakeSuffixKey(other.start.Value(idx), idx)
		sp.startBoundary = other.startBoundary
		// TODO: This is a special case to consider if we try to combine this
		// with normal spans. And it may not be correct in all cases, e.g.,
		// can a span like (/1/1/5 - /1/2/7) exist? I think it can.
		if isLastVal := other.start.Length() == idx+1; !isLastVal {
			sp.startBoundary = IncludeBoundary
		}
		mutated = true
	}
	if cmpEnds > 0 {
		sp.end = MakeSuffixKey(other.end.Value(idx), idx)
		sp.endBoundary = other.endBoundary
		// TODO: This is a special case to consider if we try to combine this
		// with normal spans. And it may not be correct in all cases, e.g.,
		// can a span like (/1/1/5 - /1/2/7) exist? I think it can.
		if isLastVal := other.end.Length() == idx+1; !isLastVal {
			sp.endBoundary = IncludeBoundary
		}
		mutated = true
	}
	return true, mutated
}

// PreferInclusive tries to convert exclusive keys to inclusive keys. This is
// only possible if the relevant type supports Next/Prev.
//
// We prefer inclusive constraints because we can extend inclusive constraints
// with more constraints on columns that follow.
//
// Examples:
//   - for an integer column (/1 - /5)  =>  [/2 - /4].
//   - for a descending integer column (/5 - /1) => (/4 - /2).
//   - for a string column, we don't have Prev so
//     (/foo - /qux)  =>  [/foo\x00 - /qux).
//   - for a decimal column, we don't have either Next or Prev so we can't
//     change anything.
func (sp *SuffixSpan) PreferInclusive(keyCtx *KeyContext) {
	if sp.startBoundary == ExcludeBoundary {
		if key, ok := sp.start.Next(keyCtx); ok {
			sp.start = key
			sp.startBoundary = IncludeBoundary
		}
	}
	if sp.endBoundary == ExcludeBoundary {
		if key, ok := sp.end.Prev(keyCtx); ok {
			sp.end = key
			sp.endBoundary = IncludeBoundary
		}
	}
}

func (sp *SuffixSpan) startExt() KeyExtension {
	// Trivial cast of start boundary value:
	//   IncludeBoundary (false) = ExtendLow (false)
	//   ExcludeBoundary (true)  = ExtendHigh (true)
	return KeyExtension(sp.startBoundary)
}

func (sp *SuffixSpan) endExt() KeyExtension {
	// Invert end boundary value:
	//   IncludeBoundary (false) = ExtendHigh (true)
	//   ExcludeBoundary (true)  = ExtendLow (false)
	return KeyExtension(!sp.endBoundary)
}

type SuffixKey struct {
	// firstVal stores the first value in the key. Subsequent values are stored
	// in otherVals. Inlining the first value avoids an extra allocation in the
	// common case of a single value in the key.
	val tree.Datum

	// TODO
	index int
}

// MakeSuffixKey constructs a simple one dimensional key having the given value. If
// val is nil, then MakeKey returns an empty key.
func MakeSuffixKey(val tree.Datum, index int) SuffixKey {
	return SuffixKey{
		val:   val,
		index: index,
	}
}

func (k SuffixKey) Value() tree.Datum {
	return k.val
}

func (k SuffixKey) Compare(keyCtx *KeyContext, l SuffixKey, kext, lext KeyExtension) int {
	// TODO: Is it weird to use k.index?
	if cmp := keyCtx.Compare(k.index, k.val, l.val); cmp != 0 {
		return cmp
	}

	// Equal keys:
	//   k (ExtendLow)  vs. l (ExtendLow)   ->  equal   (0)
	//   k (ExtendLow)  vs. l (ExtendHigh)  ->  smaller (-1)
	//   k (ExtendHigh) vs. l (ExtendLow)   ->  bigger  (1)
	//   k (ExtendHigh) vs. l (ExtendHigh)  ->  equal   (0)
	if kext == lext {
		return 0
	}
	return kext.ToCmp()
}

func (k SuffixKey) CompareToKey(keyCtx *KeyContext, l Key, kext, lext KeyExtension) int {
	klen := k.index + 1
	llen := l.Length()
	// Assume the prefix values up to the index-th value match.
	// for i := 0; i < klen && i < llen; i++ {
	// 	if cmp := keyCtx.Compare(i, k.Value(i), l.Value(i)); cmp != 0 {
	// 		return cmp
	// 	}
	// }

	// TODO(mgartner): Audit this logic and make sure it is correct.
	if klen > llen {
		// Inverse case of above.
		return -lext.ToCmp()
	}

	// TODO
	if cmp := keyCtx.Compare(k.index, k.val, l.Value(k.index)); cmp != 0 {
		return cmp
	}

	if klen < llen {
		// k matches a prefix of l:
		//   k = /1
		//   l = /1/2
		// Which of these is "smaller" depends on whether k is extended with
		// -inf or with +inf:
		//   k (ExtendLow)  = /1/Low  < /1/2  ->  k is smaller (-1)
		//   k (ExtendHigh) = /1/High > /1/2  ->  k is bigger  (1)
		return kext.ToCmp()
	}

	// Equal keys:
	//   k (ExtendLow)  vs. l (ExtendLow)   ->  equal   (0)
	//   k (ExtendLow)  vs. l (ExtendHigh)  ->  smaller (-1)
	//   k (ExtendHigh) vs. l (ExtendLow)   ->  bigger  (1)
	//   k (ExtendHigh) vs. l (ExtendHigh)  ->  equal   (0)
	if kext == lext {
		return 0
	}
	return kext.ToCmp()
}

// Next returns the next key; this only works for discrete types like integers.
// It is guaranteed that there are no  possible keys in the span
//
//	( key, Next(keu) ).
//
// Examples:
//
//	Next(/1/2) = /1/3
//	Next(/1/false) = /1/true
//	Next(/1/true) returns !ok
//	Next(/'foo') = /'foo\x00'
//
// If a column is descending, the values on that column go backwards:
//
//	Next(/2) = /1
//
// The key cannot be empty.
func (k SuffixKey) Next(keyCtx *KeyContext) (_ SuffixKey, ok bool) {
	if nextVal, ok := keyCtx.Next(k.index, k.val); ok {
		return SuffixKey{val: nextVal}, true
	}
	return SuffixKey{}, false
}

// Prev returns the next key; this only works for discrete types like integers.
//
// Examples:
//
//	Prev(/1/2) = /1/1
//	Prev(/1/true) = /1/false
//	Prev(/1/false) returns !ok.
//	Prev(/'foo') returns !ok.
//
// If a column is descending, the values on that column go backwards:
//
//	Prev(/1) = /2
//
// If this is the minimum possible key, returns EmptyKey.
func (k SuffixKey) Prev(keyCtx *KeyContext) (_ SuffixKey, ok bool) {
	if nextVal, ok := keyCtx.Prev(k.index, k.val); ok {
		return SuffixKey{val: nextVal}, true
	}
	return SuffixKey{}, false
}
