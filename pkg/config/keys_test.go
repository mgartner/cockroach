// Copyright 2015 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package config_test

import (
	"bytes"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/config"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
)

func TestDecodeSystemTenantObjectID(t *testing.T) {
	defer leaktest.AfterTest(t)()

	testCases := []struct {
		key       roachpb.RKey
		keySuffix []byte
		success   bool
		id        config.ObjectID
	}{
		// Before the structured span.
		{roachpb.RKeyMin, nil, false, 0},

		// Boundaries of structured span.
		{roachpb.RKeyMax, nil, false, 0},

		// Valid, even if there are things after the ID.
		{testutils.MakeKey(keys.SystemSQLCodec.TablePrefix(42), roachpb.RKey("\xff")), []byte{'\xff'}, true, 42},
		{roachpb.RKey(keys.SystemSQLCodec.TablePrefix(0)), []byte{}, true, 0},
		{roachpb.RKey(keys.SystemSQLCodec.TablePrefix(999)), []byte{}, true, 999},
	}

	for tcNum, tc := range testCases {
		id, keySuffix, success := config.DecodeObjectID(keys.SystemSQLCodec, tc.key)
		if success != tc.success {
			t.Errorf("#%d: expected success=%t", tcNum, tc.success)
			continue
		}
		if id != tc.id {
			t.Errorf("#%d: expected id=%d, got %d", tcNum, tc.id, id)
		}
		if !bytes.Equal(keySuffix, tc.keySuffix) {
			t.Errorf("#%d: expected suffix=%q, got %q", tcNum, tc.keySuffix, keySuffix)
		}
	}
}
