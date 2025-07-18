// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package schemachangerccl

import (
	"context"
	gosql "database/sql"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/ccl/multiregionccl/multiregionccltestutils"
	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scexec"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/sctest"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
)

// MultiRegionTestClusterFactory is a multi-region implementation of the
// sctest.TestServerFactory interface.
type MultiRegionTestClusterFactory struct {
	scexec               *scexec.TestingKnobs
	server               *server.TestingKnobs
	schemaLockedDisabled bool
}

var _ sctest.TestServerFactory = MultiRegionTestClusterFactory{}

// WithSchemaChangerKnobs implements the sctest.TestServerFactory interface.
func (f MultiRegionTestClusterFactory) WithSchemaChangerKnobs(
	knobs *scexec.TestingKnobs,
) sctest.TestServerFactory {
	f.scexec = knobs
	return f
}

// WithMixedVersion implements the sctest.TestServerFactory interface.
func (f MultiRegionTestClusterFactory) WithMixedVersion() sctest.TestServerFactory {
	f.server = &server.TestingKnobs{
		ClusterVersionOverride:         sctest.OldVersionKey.Version(),
		DisableAutomaticVersionUpgrade: make(chan struct{}),
	}
	return f
}

// WithSchemaLockDisabled implements the sctest.TestServerFactory interface.
func (f MultiRegionTestClusterFactory) WithSchemaLockDisabled() sctest.TestServerFactory {
	f.schemaLockedDisabled = true
	return f
}

// Run implements the sctest.TestServerFactory interface.
func (f MultiRegionTestClusterFactory) Run(
	ctx context.Context, t *testing.T, fn func(_ serverutils.TestServerInterface, _ *gosql.DB),
) {
	const numServers = 3
	knobs := base.TestingKnobs{
		SQLEvalContext: &eval.TestingKnobs{
			ForceProductionValues: true,
		},
		JobsTestingKnobs: jobs.NewTestingKnobsWithShortIntervals(),
		SQLExecutor: &sql.ExecutorTestingKnobs{
			UseTransactionalDescIDGenerator: true,
		},
	}
	if f.server != nil {
		knobs.Server = f.server
	}
	if f.scexec != nil {
		knobs.SQLDeclarativeSchemaChanger = f.scexec
	}
	// Always run this test with schema_locked by default.
	st := cluster.MakeTestingClusterSettings()
	if f.server != nil && f.server.ClusterVersionOverride.Major != 0 {
		st = cluster.MakeClusterSettingsWithVersions(clusterversion.Latest.Version(), f.server.ClusterVersionOverride)
	}
	sql.CreateTableWithSchemaLocked.Override(ctx, &st.SV, !f.schemaLockedDisabled)
	c, db, _ := multiregionccltestutils.TestingCreateMultiRegionCluster(t, numServers, knobs, multiregionccltestutils.WithSettings(st))
	defer c.Stopper().Stop(ctx)
	fn(c.Server(0), db)
}
