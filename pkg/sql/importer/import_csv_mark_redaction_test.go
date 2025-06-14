// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package importer

import (
	"testing"

	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/stretchr/testify/require"
)

// TestMarkRedactionCCLStatement verifies that the redactable parts
// of CCL statements are marked correctly.
func TestMarkRedactionCCLStatement(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	testCases := []struct {
		query    string
		expected string
	}{
		{
			`IMPORT INTO foo CSV DATA ('file') WITH delimiter = 'foo'`,
			`IMPORT INTO ‹""›.‹""›.‹foo› CSV DATA (‹'*****'›) WITH OPTIONS (delimiter = ‹'foo'›)`,
		},
	}

	for _, test := range testCases {
		stmt, err := parser.ParseOne(test.query)
		require.NoError(t, err)
		annotations := tree.MakeAnnotations(stmt.NumAnnotations)
		f := tree.NewFmtCtx(tree.FmtAlwaysQualifyTableNames|tree.FmtMarkRedactionNode, tree.FmtAnnotations(&annotations))
		f.FormatNode(stmt.AST)
		redactedString := f.CloseAndGetString()
		require.Equal(t, test.expected, redactedString)
	}
}
