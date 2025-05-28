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
	srcStores := storeDirs(src)
	dstStores := storeDirs(dst)
	for i := range srcStores {
		if _, err := vfs.Clone(vfs.Default, vfs.Default, srcStores[i], dstStores[i]); err != nil {
			t.Fatal(err)
		}
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

	stores := storeDirs(storeDir)
	serverArgs := make(map[int]base.TestServerArgs, nodes)
	for i := 0; i < nodes; i++ {
		serverArgs[i] = base.TestServerArgs{
			StoreSpecs: []base.StoreSpec{{Path: stores[i]}},
		}
	}
	cluster := serverutils.StartCluster(t, nodes, base.TestClusterArgs{
		ServerArgsPerNode: serverArgs,
		ParallelStart:     true,
	})
	defer cluster.Stopper().Stop(ctx)

	tdb := sqlutils.MakeSQLRunner(cluster.ServerConn(0))
	tdb.Exec(t, "CREATE DATABASE "+dbName)
	tdb.Exec(t, "USE "+dbName)
	tpcc, err := workload.Get("tpcc")
	if err != nil {
		t.Fatal(err)
	}
	gen := tpcc.New().(interface {
		workload.Flagser
		workload.Hookser
		workload.Generator
	})
	if err := gen.Flags().Parse([]string{"--db=" + dbName}); err != nil {
		t.Fatal(err)
	}
	if err := gen.Hooks().Validate(); err != nil {
		t.Fatal(err)
	}

	var l workloadsql.InsertsDataLoader
	if _, err := workloadsql.Setup(ctx, cluster.ServerConn(0), gen, l); err != nil {
		t.Fatal(err)
	}
}
