// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tpcc

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/testutils/pgurlutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/testfixtures"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/workload"
	"github.com/cockroachdb/cockroach/pkg/workload/histogram"
	_ "github.com/cockroachdb/cockroach/pkg/workload/tpcc"
	"github.com/jackc/pgx/v5"
)

const (
	dbName = "tpcc"
)

// BenchmarkTPCC runs TPC-C transactions against a single warehouse. It runs the
// client side of the workload in a subprocess so that the client overhead is
// not included in CPU and heap profiles.
//
// The benchmark will generate the schema and table data for a single warehouse,
// using a reusable store directory. In future runs the cockroach server will
// clone and use the store directory, rather than regenerating the schema and
// data. This enables faster iteration when re-running the benchmark.
func BenchmarkTPCC(b *testing.B) {
	defer log.Scope(b).Close(b)

	// Reuse or generate TPCC data.
	storeName := "bench_tpcc_store_" + storage.MinimumSupportedFormatVersion.String()
	storeDir := testfixtures.ReuseOrGenerate(b, storeName, func(dir string) {
		c, output := generateStoreDir.withEnv(storeDirEnvVar, dir).exec()
		if err := c.Run(); err != nil {
			b.Fatalf("failed to generate store dir: %s\n%s", err, output.String())
		}
	})

	for _, impl := range []struct{ name, flag string }{
		{"literal", "--literal-implementation=true"},
		{"optimized", "--literal-implementation=false"},
	} {
		b.Run(impl.name, func(b *testing.B) {
			for _, mix := range []struct{ name, flag string }{
				{"new_order", "--mix=newOrder=1"},
				{"payment", "--mix=payment=1"},
				{"order_status", "--mix=orderStatus=1"},
				{"delivery", "--mix=delivery=1"},
				{"stock_level", "--mix=stockLevel=1"},
				{"default", "--mix=newOrder=10,payment=10,orderStatus=1,delivery=1,stockLevel=1"},
			} {
				b.Run(mix.name, func(b *testing.B) {
					run(b, storeDir, []string{impl.flag, mix.flag})
				})
			}
		})

	}
}

func run(b *testing.B, storeDir string, workloadFlags []string) {
	ctx := context.Background()

	server, pgURL := startCockroach(b, storeDir)
	defer server.Stopper().Stop(context.Background())

	verifyDatabaseExists(b, pgURL)

	tpcc, err := workload.Get("tpcc")
	if err != nil {
		b.Fatal(err)
	}
	gen := tpcc.New()
	wl := gen.(interface {
		workload.Flagser
		workload.Hookser
		workload.Opser
	})

	flags := append([]string{
		"--wait=0",
		"--workers=1",
		"--db=" + dbName,
	}, workloadFlags...)
	if err := wl.Flags().Parse(flags); err != nil {
		b.Fatal(err)
	}
	if err := wl.Hooks().Validate(); err != nil {
		b.Fatal(err)
	}

	// Temporarily redirect stdout to /dev/null.
	restore := redirectStdoutToDevNull(b)
	defer restore()

	reg := histogram.NewRegistry(time.Minute, "tpcc")
	ql, err := wl.Ops(ctx, []string{pgURL}, reg)
	if err != nil {
		b.Fatal(b)
	}
	defer func() { _ = ql.Close(ctx) }()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ql.WorkerFns[0](ctx); err != nil {
			b.Fatalf("worker function failed: %s", err)
		}
	}
	b.StopTimer()
}

// startCockroach clones the store directory and starts a CockroachDB server.
func startCockroach(
	b testing.TB, storeDir string,
) (server serverutils.TestServerInterface, pgURL string) {
	// Clone the store dir.
	td := b.TempDir()
	c, output := cloneEngine.
		withEnv(srcEngineEnvVar, storeDir).
		withEnv(dstEngineEnvVar, td).
		exec()
	if err := c.Run(); err != nil {
		b.Fatalf("failed to clone engine: %s\n%s", err, output.String())
	}

	// Start the server.
	s := serverutils.StartServerOnly(b, base.TestServerArgs{
		StoreSpecs: []base.StoreSpec{{Path: td}},
	})

	// Generate a PG URL.
	u, urlCleanup, err := pgurlutils.PGUrlE(
		s.AdvSQLAddr(), b.TempDir(), url.User("root"),
	)
	if err != nil {
		b.Fatalf("failed to create pgurl: %s", err)
	}
	u.Path = dbName
	s.Stopper().AddCloser(stop.CloserFn(urlCleanup))

	return s, u.String()
}

// verifyDatabaseExists checks if the tpcc database exists and is accessible.
func verifyDatabaseExists(b *testing.B, pgURL string) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pgURL)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "USE "+dbName); err != nil {
		b.Fatalf("database %q does not exist", dbName)
	}
}

// redirectStdoutToDevNull redirects stdout to /dev/null to suppress the
// workload's output during the benchmark.
func redirectStdoutToDevNull(b *testing.B) (restore func()) {
	old := os.Stdout
	var err error
	if os.Stdout, err = os.Open(os.DevNull); err != nil {
		b.Fatal(err)
	}
	return func() {
		_ = os.Stdout.Close()
		os.Stdout = old
	}
}
