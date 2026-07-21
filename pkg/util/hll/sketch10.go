// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package hll

import (
	"math"
	"math/bits"
)

const (
	// Precision.
	p = 10
	// Number of registers.
	m = 1 << p
	// TODO
	alpha = 0.7213 / (1 + 1.079/float64(m))
)

type Sketch10 struct {
	rs [m]uint8
}

func (sk *Sketch10) Add(x uint64) {
	x = splitmix64Finalizer(x)
	i := index(x)
	z := leadingZeros(x)
	r := sk.rs[i]
	if z > r {
		sk.rs[i] = z
	}
}

func (sk *Sketch10) Cardinality() uint64 {
	sum, ez := sk.sumAndZeros()
	est := alpha * m * (m - ez) / (sum + beta10(ez))
	return uint64(est + 0.5)
}

func index(x uint64) uint64 {
	// The first 64-p bits are the register index.
	return x >> (64 - p)
}

func leadingZeros(x uint64) uint8 {
	w := x<<p | 1<<(p-1)
	return uint8(bits.LeadingZeros64(w)) + 1
}

// pow2Inverse[b] stores 1.0 / (2^b) for all b in [0, 255].
// NOTE: We don't
// TODO: This is an easy change to make upstrem without any
// API change required.
var pow2Inverse = func() [256]float64 {
	var table [256]float64
	// for i := range table {
	for i := 0; i <= 64-p; i++ {
		table[i] = 1.0 / math.Pow(2.0, float64(i))
	}
	return table
}()

// TODO: Would it be relatively cheap to keep a running sum of ez and sum?
func (sk *Sketch10) sumAndZeros() (sum, ez float64) {
	for _, v := range sk.rs {
		if v == 0 {
			ez++
		}
		sum += pow2Inverse[v]
	}
	return sum, ez
}

func beta10(ez float64) float64 {
	zl := math.Log(ez + 1)
	return -0.25935400670790054*ez +
		-0.52598301999805808*zl +
		1.48933034925876839*math.Pow(zl, 2) +
		-1.29642714084993571*math.Pow(zl, 3) +
		0.62284756217221615*math.Pow(zl, 4) +
		-0.15672326770251041*math.Pow(zl, 5) +
		0.02054415903878563*math.Pow(zl, 6) +
		-0.00112488483925502*math.Pow(zl, 7)
}

// Perform variant 13 of David Stafford's 64-bit mix function.
// This is the mix function used in the
// {@link org.apache.commons.rng.core.source64.SplitMix64 SplitMix64} RNG.
//
// This is ranked first of the top 14 Stafford mixers.
//
// @param x the input value
// @return the output value
// Bit Mixing - Improving on MurmurHash3's 64-bit Finalizer.</a>
// https://zimbry.blogspot.com/2011/09/better-bit-mixing-improving-on.html
func splitmix64Finalizer(x uint64) uint64 {
	x = (x ^ (x >> 33)) * 0xff51afd7ed558ccd
	x = (x ^ (x >> 33)) * 0xc4ceb9fe1a85ec53
	x = (x ^ (x >> 33))
	return x
}
