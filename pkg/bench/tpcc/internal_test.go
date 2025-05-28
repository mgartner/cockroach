// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tpcc

import (
	"context"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/envutil"
	"github.com/cockroachdb/cockroach/pkg/workload"
	"github.com/cockroachdb/cockroach/pkg/workload/workloadsql"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/stretchr/testify/require"
)

// This file contains "internal tests" that are run by BenchmarkTPCC in a
// subprocess. They are not real tests at all, and they are skipped if the
// COCKROACH_INTERNAL_TEST environment variable is not set. These tests are run
// in a subprocess so that profiles collected while running the benchmark do not
// include the overhead of the client code.

// Environment variables used to communicate configuration from the benchmark
// to the subprocesses.
const (
	allowInternalTestEnvVar = "COCKROACH_INTERNAL_TEST"
	storeDirEnvVar          = "COCKROACH_STORE_DIR"
	srcEngineEnvVar         = "COCKROACH_SRC_ENGINE"
	dstEngineEnvVar         = "COCKROACH_DST_ENGINE"
)

var (
	allowInternalTest = envutil.EnvOrDefaultBool(allowInternalTestEnvVar, false)

	cloneEngine      = makeCmd("TestInternalCloneEngine", TestInternalCloneEngine)
	generateStoreDir = makeCmd("TestInternalGenerateStoreDir", TestInternalGenerateStoreDir)
)

func TestInternalCloneEngine(t *testing.T) {
	if !allowInternalTest {
		skip.IgnoreLint(t)
	}

	src, ok := envutil.EnvString(srcEngineEnvVar, 0)
	if !ok {
		t.Fatal("missing src engine env var")
	}
	dst, ok := envutil.EnvString(dstEngineEnvVar, 0)
	if !ok {
		t.Fatal("missing dst engine env var")
	}
	if _, err := vfs.Clone(vfs.Default, vfs.Default, src, dst); err != nil {
		t.Fatal(err)
	}
}

func TestInternalGenerateStoreDir(t *testing.T) {
	if !allowInternalTest {
		skip.IgnoreLint(t)
	}

	ctx := context.Background()
	storeDir, ok := envutil.EnvString(storeDirEnvVar, 0)
	if !ok {
		t.Fatal("missing store dir env var")
	}

	srv, db, _ := serverutils.StartServer(t, base.TestServerArgs{
		StoreSpecs: []base.StoreSpec{{Path: storeDir}},
	})
	defer srv.Stopper().Stop(ctx)

	tdb := sqlutils.MakeSQLRunner(db)
	tdb.Exec(t, "CREATE DATABASE "+dbName)
	tdb.Exec(t, "USE "+dbName)
	tpcc, err := workload.Get("tpcc")
	require.NoError(t, err)
	gen := tpcc.New().(interface {
		workload.Flagser
		workload.Hookser
		workload.Generator
	})
	require.NoError(t, gen.Flags().Parse([]string{
		"--db=" + dbName,
	}))
	require.NoError(t, gen.Hooks().Validate())
	{
		var l workloadsql.InsertsDataLoader
		_, err := workloadsql.Setup(ctx, db, gen, l)
		require.NoError(t, err)
	}
}
