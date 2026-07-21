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
	"testing"
)

func TestSketch10(t *testing.T) {
	var sk Sketch10

	n := sk.Cardinality()
	if n != 0 {
		t.Errorf("expected cardinality of 0, got %d", n)
	}

	sk.Add(0x00010fffffffffff)
	sk.Add(0x00020fffffffffff)
	sk.Add(0x00030fffffffffff)
	sk.Add(0x00040fffffffffff)
	sk.Add(0x00050fffffffffff)
	sk.Add(0x00050fffffffffff)
	if c := sk.Cardinality(); c != 5 {
		t.Errorf("expected cardinality of 5, got %d", n)
	}

	sk.Add(0x00010fffffffffff)
	sk.Add(0x00020fffffffffff)
	sk.Add(0x00030fffffffffff)
	sk.Add(0x00040fffffffffff)
	sk.Add(0x00050fffffffffff)
	sk.Add(0x00050fffffffffff)
	if c := sk.Cardinality(); c != 5 {
		t.Errorf("expected cardinality of 5, got %d", n)
	}

	sk.Add(0x10010f00ffffffff)
	sk.Add(0x20020f00ffffffff)
	sk.Add(0x30030f00ffffffff)
	sk.Add(0x40040f00ffffffff)
	sk.Add(0x50050f00ffffffff)
	sk.Add(0x60050f00ffffffff)
	if c := sk.Cardinality(); c != 11 {
		t.Errorf("expected cardinality of 11, got %d", n)
	}

	sk.Add(0x00060fffffffffff)
	if c := sk.Cardinality(); c != 12 {
		t.Errorf("expected cardinality of 12, got %d", n)
	}
}
