// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package split

import (
	"context"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testLoadSplitConfig implements the LoadSplitConfig interface and may be used
// in testing.
type testLoadSplitConfig struct {
	randSource          RandSource
	useWeighted         bool
	statRetention       time.Duration
	statThreshold       float64
	sampleResetDuration time.Duration
}

// NewLoadBasedSplitter returns a new LoadBasedSplitter that may be used to
// find the midpoint based on recorded load.
func (t *testLoadSplitConfig) NewLoadBasedSplitter(
	startTime time.Time, _ SplitObjective,
) LoadBasedSplitter {
	if t.useWeighted {
		return NewWeightedFinder(startTime, t.randSource)
	}
	return NewUnweightedFinder(startTime, t.randSource)
}

// StatRetention returns the duration that recorded load is to be retained.
func (t *testLoadSplitConfig) StatRetention() time.Duration {
	return t.statRetention
}

// StatThreshold returns the threshold for load above which the range
// should be considered split.
func (t *testLoadSplitConfig) StatThreshold(_ SplitObjective) float64 {
	return t.statThreshold
}

// SampleResetDuration returns the duration that any sampling structure should
// retain data for before resetting.
func (t *testLoadSplitConfig) SampleResetDuration() time.Duration {
	return t.sampleResetDuration
}

func ld(n int) func(SplitObjective) int {
	return func(_ SplitObjective) int {
		return n
	}
}

func ms(i int) time.Time {
	ts, err := time.Parse(time.RFC3339, "2000-01-01T00:00:00Z")
	if err != nil {
		panic(err)
	}
	return ts.Add(time.Duration(i) * time.Millisecond)
}

func newSplitterMetrics() *LoadSplitterMetrics {
	return &LoadSplitterMetrics{
		PopularKeyCount:     metric.NewCounter(metric.Metadata{}),
		NoSplitKeyCount:     metric.NewCounter(metric.Metadata{}),
		ClearDirectionCount: metric.NewCounter(metric.Metadata{}),
	}
}

func TestDecider(t *testing.T) {
	defer leaktest.AfterTest(t)()

	rng := rand.New(rand.NewPCG(12, 12))
	loadSplitConfig := testLoadSplitConfig{
		randSource:    rng,
		useWeighted:   false,
		statRetention: 2 * time.Second,
		statThreshold: 10,
	}

	var d Decider
	Init(&d, &loadSplitConfig, newSplitterMetrics(),
		SplitQPS,
	)

	op := func(s string) func() roachpb.Span {
		return func() roachpb.Span { return roachpb.Span{Key: roachpb.Key(s)} }
	}

	assertStat := func(i int, expStat float64) {
		t.Helper()
		stat := d.lastStatLocked(context.Background(), ms(i))
		assert.Equal(t, expStat, stat)
	}

	assertMaxStat := func(i int, expMaxStat float64, expOK bool) {
		t.Helper()
		maxStat, ok := d.maxStatLocked(context.Background(), ms(i))
		assert.Equal(t, expMaxStat, maxStat)
		assert.Equal(t, expOK, ok)
	}

	assert.Equal(t, false, d.Record(context.Background(), ms(100), ld(1), nil))
	assertStat(100, 0)
	assertMaxStat(100, 0, false)

	assert.Equal(t, ms(100), d.mu.lastStatRollover)
	assert.EqualValues(t, 1, d.mu.count)

	assert.Equal(t, false, d.Record(context.Background(), ms(400), ld(3), nil))
	assertStat(100, 0)
	assertStat(700, 0)
	assertMaxStat(400, 0, false)

	assert.Equal(t, false, d.Record(context.Background(), ms(300), ld(3), nil))
	assertStat(100, 0)
	assertMaxStat(300, 0, false)

	assert.Equal(t, false, d.Record(context.Background(), ms(900), ld(1), nil))
	assertStat(0, 0)
	assertMaxStat(900, 0, false)

	assert.Equal(t, false, d.Record(context.Background(), ms(1099), ld(1), nil))
	assertStat(0, 0)
	assertMaxStat(1099, 0, false)

	// Now 9 operations happened in the interval [100, 1099]. The next higher
	// timestamp will decide whether to engage the split finder.

	// It won't engage because the duration between the rollovers is 1.1s, and
	// we had 10 events over that interval.
	assert.Equal(t, false, d.Record(context.Background(), ms(1200), ld(1), nil))
	assertStat(0, float64(10)/float64(1.1))
	assert.Equal(t, ms(1200), d.mu.lastStatRollover)
	assertMaxStat(1099, 0, false)

	assert.Equal(t, nil, d.mu.splitFinder)

	assert.Equal(t, false, d.Record(context.Background(), ms(2199), ld(12), nil))
	assert.Equal(t, nil, d.mu.splitFinder)

	// 2200 is the next rollover point, and 12+1=13 stat should be computed.
	assert.Equal(t, false, d.Record(context.Background(), ms(2200), ld(1), op("a")))
	assert.Equal(t, ms(2200), d.mu.lastStatRollover)
	assertStat(0, float64(13))
	assertMaxStat(2200, 13, true)

	assert.NotNil(t, d.mu.splitFinder)
	assert.False(t, d.mu.splitFinder.Ready(ms(10)))

	// With continued partitioned write load, split finder eventually tells us
	// to split. We don't test the details of exactly when that happens because
	// this is done in the finder tests.
	tick := 2200
	for o := op("a"); !d.Record(context.Background(), ms(tick), ld(11), o); tick += 1000 {
		if tick/1000%2 == 0 {
			o = op("z")
		} else {
			o = op("a")
		}
	}

	assert.Equal(t, roachpb.Key("z"), d.MaybeSplitKey(context.Background(), ms(tick)))

	// We were told to split, but won't be told to split again for some time
	// to avoid busy-looping on split attempts.
	for i := 0; i <= int(minSplitSuggestionInterval/time.Second); i++ {
		o := op("z")
		if i%2 != 0 {
			o = op("a")
		}
		assert.False(t, d.Record(context.Background(), ms(tick), ld(11), o))
		assert.True(t, d.lastStatLocked(context.Background(), ms(tick)) > 1.0)
		// Even though the split key remains.
		assert.Equal(t, roachpb.Key("z"), d.MaybeSplitKey(context.Background(), ms(tick+999)))
		tick += 1000
	}
	// But after minSplitSuggestionInterval of ticks, we get another one.
	assert.True(t, d.Record(context.Background(), ms(tick), ld(11), op("a")))
	assertStat(tick, float64(11))
	assertMaxStat(tick, 11, true)

	// Split key suggestion vanishes once stat drops.
	tick += 1000
	assert.False(t, d.Record(context.Background(), ms(tick), ld(9), op("a")))
	assert.Equal(t, roachpb.Key(nil), d.MaybeSplitKey(context.Background(), ms(tick)))
	assert.Equal(t, nil, d.mu.splitFinder)

	// Hammer a key with writes above threshold. There shouldn't be a split
	// since everyone is hitting the same key and load can't be balanced.
	for i := 0; i < 1000; i++ {
		assert.False(t, d.Record(context.Background(), ms(tick), ld(11), op("q")))
		tick += 1000
	}
	assert.True(t, d.mu.splitFinder.Ready(ms(tick)))
	assert.Equal(t, roachpb.Key(nil), d.MaybeSplitKey(context.Background(), ms(tick)))

	// But the finder keeps sampling to adapt to changing workload...
	for i := 0; i < 1000; i++ {
		assert.False(t, d.Record(context.Background(), ms(tick), ld(11), op("p")))
		tick += 1000
	}

	// ... which we verify by looking at its samples directly.
	for _, sample := range d.mu.splitFinder.(*UnweightedFinder).samples {
		assert.Equal(t, roachpb.Key("p"), sample.key)
	}

	// Since the new workload is also not partitionable, nothing changes in
	// the decision.
	assert.True(t, d.mu.splitFinder.Ready(ms(tick)))
	assert.Equal(t, roachpb.Key(nil), d.MaybeSplitKey(context.Background(), ms(tick)))

	// Get the decider engaged again so that we can test Reset().
	for i := 0; i < 1000; i++ {
		o := op("z")
		if i%2 != 0 {
			o = op("a")
		}
		d.Record(context.Background(), ms(tick), ld(11), o)
		tick += 500
	}

	// The finder wants to split, until Reset is called, at which point it starts
	// back up at zero.
	assert.True(t, d.mu.splitFinder.Ready(ms(tick)))
	assert.Equal(t, roachpb.Key("z"), d.MaybeSplitKey(context.Background(), ms(tick)))
	d.Reset(ms(tick))
	assert.Nil(t, d.MaybeSplitKey(context.Background(), ms(tick)))
	assert.Nil(t, d.mu.splitFinder)
}

func TestDecider_MaxStat(t *testing.T) {
	defer leaktest.AfterTest(t)()

	rng := rand.New(rand.NewPCG(11, 11))
	loadSplitConfig := testLoadSplitConfig{
		randSource:    rng,
		useWeighted:   false,
		statRetention: 10 * time.Second,
		statThreshold: 100,
	}

	var d Decider
	Init(&d, &loadSplitConfig, newSplitterMetrics(), SplitQPS)

	assertMaxStat := func(i int, expMaxStat float64, expOK bool) {
		t.Helper()
		maxStat, ok := d.maxStatLocked(context.Background(), ms(i))
		assert.Equal(t, expMaxStat, maxStat)
		assert.Equal(t, expOK, ok)
	}

	assertMaxStat(1000, 0, false)

	// Record a large number of samples.
	d.Record(context.Background(), ms(1500), ld(5), nil)
	d.Record(context.Background(), ms(2000), ld(5), nil)
	d.Record(context.Background(), ms(4500), ld(1), nil)
	d.Record(context.Background(), ms(5000), ld(15), nil)
	d.Record(context.Background(), ms(5500), ld(2), nil)
	d.Record(context.Background(), ms(8000), ld(5), nil)
	d.Record(context.Background(), ms(10000), ld(9), nil)

	assertMaxStat(10000, 0, false)
	assertMaxStat(11000, 17, true)

	// Record more samples with a lower Stat.
	d.Record(context.Background(), ms(12000), ld(1), nil)
	d.Record(context.Background(), ms(13000), ld(4), nil)
	d.Record(context.Background(), ms(15000), ld(2), nil)
	d.Record(context.Background(), ms(19000), ld(3), nil)

	assertMaxStat(20000, 4.5, true)
	assertMaxStat(21000, 4, true)

	// Add in a few Stat reading directly.
	d.RecordMax(ms(24000), 6)

	assertMaxStat(25000, 6, true)
}

func TestMaxStatTracker(t *testing.T) {
	defer leaktest.AfterTest(t)()

	tick := 100
	minRetention := time.Second

	var mt maxStatTracker
	mt.reset(ms(tick), minRetention)
	require.Equal(t, 200*time.Millisecond, mt.windowWidth())

	// Check the maxStat returns false before any samples are recorded.
	stat, ok := mt.max(ms(tick), minRetention)
	require.Equal(t, 0.0, stat)
	require.Equal(t, false, ok)
	require.Equal(t, [6]float64{0, 0, 0, 0, 0, 0}, mt.windows)
	require.Equal(t, 0, mt.curIdx)

	// Add some samples, but not for the full minRetention period. Each window
	// should contain 4 samples.
	for i := 0; i < 15; i++ {
		tick += 50
		mt.record(ms(tick), minRetention, float64(10+i))
	}

	// maxStat should still return false, but some windows should have samples.
	stat, ok = mt.max(ms(tick), minRetention)
	require.Equal(t, 0.0, stat)
	require.Equal(t, false, ok)
	require.Equal(t, [6]float64{12, 16, 20, 24, 0, 0}, mt.windows)
	require.Equal(t, 3, mt.curIdx)

	// Add some more samples.
	for i := 0; i < 15; i++ {
		tick += 50
		mt.record(ms(tick), minRetention, float64(24+i))
	}

	// maxStat should now return the maximum stat observed during the measurement
	// period.
	stat, ok = mt.max(ms(tick), minRetention)
	require.Equal(t, 38.0, stat)
	require.Equal(t, true, ok)
	require.Equal(t, [6]float64{35, 38, 20, 24, 27, 31}, mt.windows)
	require.Equal(t, 1, mt.curIdx)

	// Add another sample, this time with a small gap between it and the previous
	// sample, so that 2 windows are skipped.
	tick += 500
	mt.record(ms(tick), minRetention, float64(17))

	stat, ok = mt.max(ms(tick), minRetention)
	require.Equal(t, 38.0, stat)
	require.Equal(t, true, ok)
	require.Equal(t, [6]float64{35, 38, 0, 0, 17, 31}, mt.windows)
	require.Equal(t, 4, mt.curIdx)

	// A query far in the future should return 0, because this indicates no
	// recent activity.
	tick += 1900
	stat, ok = mt.max(ms(tick), minRetention)
	require.Equal(t, 0.0, stat)
	require.Equal(t, true, ok)
	require.Equal(t, [6]float64{0, 0, 0, 0, 0, 0}, mt.windows)
	require.Equal(t, 0, mt.curIdx)

	// Add some new samples, then change the retention period, Should reset
	// tracker.
	for i := 0; i < 15; i++ {
		tick += 50
		mt.record(ms(tick), minRetention, float64(33+i))
	}

	stat, ok = mt.max(ms(tick), minRetention)
	require.Equal(t, 47.0, stat)
	require.Equal(t, true, ok)
	require.Equal(t, [6]float64{35, 39, 43, 47, 0, 0}, mt.windows)
	require.Equal(t, 3, mt.curIdx)

	minRetention = 2 * time.Second
	for i := 0; i < 15; i++ {
		tick += 50
		mt.record(ms(tick), minRetention, float64(13+i))
	}

	stat, ok = mt.max(ms(tick), minRetention)
	require.Equal(t, 0.0, stat)
	require.Equal(t, false, ok)
	require.Equal(t, [6]float64{20, 27, 0, 0, 0, 0}, mt.windows)
	require.Equal(t, 1, mt.curIdx)
}

func TestSplitStatisticsGeneral(t *testing.T) {
	defer leaktest.AfterTest(t)()
	for _, test := range []struct {
		name        string
		useWeighted bool
		expected    *SplitStatistics
	}{
		{"unweighted", false, &SplitStatistics{
			AccessDirection: 0.4945791444904396,
			PopularKey: PopularKey{
				Key:       keys.SystemSQLCodec.TablePrefix(uint32(52)),
				Frequency: 0.05,
			},
		}},
		{"weighted", true, &SplitStatistics{
			AccessDirection: 0.3885786802030457,
			PopularKey: PopularKey{
				Key:       keys.SystemSQLCodec.TablePrefix(uint32(111)),
				Frequency: 0.05,
			},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rand := rand.New(rand.NewPCG(11, 11))
			timeStart := 1000

			var decider Decider
			loadSplitConfig := testLoadSplitConfig{
				randSource:    rand,
				useWeighted:   test.useWeighted,
				statRetention: time.Second,
				statThreshold: 1,
			}

			Init(&decider, &loadSplitConfig, newSplitterMetrics(), SplitCPU)

			for i := 1; i <= 1000; i++ {
				k := i
				if i > 500 {
					k = 500 - i
				}
				decider.Record(context.Background(), ms(timeStart+i*50), ld(1), func() roachpb.Span {
					return roachpb.Span{Key: keys.SystemSQLCodec.TablePrefix(uint32(k))}
				})
			}

			assert.Equal(t, decider.SplitStatistics(), test.expected)
		})
	}
}

func TestSplitStatisticsPopularKey(t *testing.T) {
	defer leaktest.AfterTest(t)()
	for _, test := range []struct {
		name        string
		useWeighted bool
		expected    *SplitStatistics
	}{
		{"unweighted", false, &SplitStatistics{
			AccessDirection: 1,
			PopularKey: PopularKey{
				Key:       keys.SystemSQLCodec.TablePrefix(uint32(100)),
				Frequency: 1,
			},
		}},
		{"weighted", true, &SplitStatistics{
			AccessDirection: 1,
			PopularKey: PopularKey{
				Key:       keys.SystemSQLCodec.TablePrefix(uint32(100)),
				Frequency: 1,
			},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rand := rand.New(rand.NewPCG(11, 11))
			timeStart := 1000

			var decider Decider
			loadSplitConfig := testLoadSplitConfig{
				randSource:    rand,
				useWeighted:   test.useWeighted,
				statRetention: time.Second,
				statThreshold: 1,
			}

			Init(&decider, &loadSplitConfig, newSplitterMetrics(), SplitCPU)

			for i := 1; i <= 1000; i++ {
				decider.Record(context.Background(), ms(timeStart+i*50), ld(1), func() roachpb.Span {
					return roachpb.Span{Key: keys.SystemSQLCodec.TablePrefix(uint32(100))}
				})
			}

			assert.Equal(t, decider.SplitStatistics(), test.expected)
		})
	}
}

func TestDeciderMetrics(t *testing.T) {
	defer leaktest.AfterTest(t)()
	rng := rand.New(rand.NewPCG(11, 11))
	timeStart := 1000

	var dPopular Decider
	loadSplitConfig := testLoadSplitConfig{
		randSource:    rng,
		useWeighted:   false,
		statRetention: time.Second,
		statThreshold: 1,
	}

	Init(&dPopular, &loadSplitConfig, newSplitterMetrics(), SplitCPU)

	// No split key, popular key, clear direction
	for i := 0; i < 20; i++ {
		dPopular.Record(context.Background(), ms(timeStart), ld(1), func() roachpb.Span {
			return roachpb.Span{Key: keys.SystemSQLCodec.TablePrefix(uint32(0))}
		})
	}
	for i := 1; i <= 2000; i++ {
		dPopular.Record(context.Background(), ms(timeStart+i*50), ld(1), func() roachpb.Span {
			return roachpb.Span{Key: keys.SystemSQLCodec.TablePrefix(uint32(0))}
		})
	}

	assert.Equal(t, dPopular.loadSplitterMetrics.PopularKeyCount.Count(), int64(2))
	assert.Equal(t, dPopular.loadSplitterMetrics.NoSplitKeyCount.Count(), int64(2))
	assert.Equal(t, dPopular.loadSplitterMetrics.ClearDirectionCount.Count(), int64(2))

	// No split key, not popular key, clear direction
	var dNotPopular Decider
	Init(&dNotPopular, &loadSplitConfig, newSplitterMetrics(), SplitCPU)

	for i := 0; i < 20; i++ {
		dNotPopular.Record(context.Background(), ms(timeStart), ld(1), func() roachpb.Span {
			return roachpb.Span{Key: keys.SystemSQLCodec.TablePrefix(uint32(0))}
		})
	}
	for i := 1; i <= 2000; i++ {
		dNotPopular.Record(context.Background(), ms(timeStart+i*50), ld(1), func() roachpb.Span {
			return roachpb.Span{Key: keys.SystemSQLCodec.TablePrefix(uint32(i))}
		})
	}

	assert.Equal(t, dNotPopular.loadSplitterMetrics.PopularKeyCount.Count(), int64(0))
	assert.Equal(t, dNotPopular.loadSplitterMetrics.NoSplitKeyCount.Count(), int64(2))
	assert.Equal(t, dNotPopular.loadSplitterMetrics.ClearDirectionCount.Count(), int64(2))

	// no split key, no popular key, no clear direction
	var dNoClearDirection Decider
	Init(&dNoClearDirection, &loadSplitConfig, newSplitterMetrics(), SplitCPU)
	for i := 0; i < 20; i++ {
		dNoClearDirection.Record(context.Background(), ms(timeStart), ld(1), func() roachpb.Span {
			return roachpb.Span{Key: keys.SystemSQLCodec.TablePrefix(uint32(i))}
		})
	}
	for i := 1; i <= 2000; i++ {
		dNoClearDirection.Record(context.Background(), ms(timeStart+i*1000), ld(1), func() roachpb.Span {
			return roachpb.Span{Key: keys.SystemSQLCodec.TablePrefix(uint32(i % 20))}
		})
	}

	assert.Equal(t, dNoClearDirection.loadSplitterMetrics.PopularKeyCount.Count(), int64(0))
	assert.Equal(t, dNoClearDirection.loadSplitterMetrics.NoSplitKeyCount.Count(), int64(0))
	assert.Equal(t, dNoClearDirection.loadSplitterMetrics.ClearDirectionCount.Count(), int64(0))

	// No split key, all insufficient counters
	var dAllInsufficientCounters Decider
	Init(&dAllInsufficientCounters, &loadSplitConfig, newSplitterMetrics(), SplitCPU)
	for i := 0; i < 20; i++ {
		dAllInsufficientCounters.Record(context.Background(), ms(timeStart), ld(1), func() roachpb.Span {
			return roachpb.Span{Key: keys.SystemSQLCodec.TablePrefix(uint32(0))}
		})
	}
	for i := 1; i <= 80; i++ {
		dAllInsufficientCounters.Record(context.Background(), ms(timeStart+i*1000), ld(1), func() roachpb.Span {
			return roachpb.Span{Key: keys.SystemSQLCodec.TablePrefix(uint32(0))}
		})
	}

	assert.Equal(t, dAllInsufficientCounters.loadSplitterMetrics.PopularKeyCount.Count(), int64(0))
	assert.Equal(t, dAllInsufficientCounters.loadSplitterMetrics.NoSplitKeyCount.Count(), int64(0))
	assert.Equal(t, dAllInsufficientCounters.loadSplitterMetrics.ClearDirectionCount.Count(), int64(0))

}

// TestDeciderSampleReset tests the sample reset functionality of the decider,
// when the sample reset duration is non-zero, the split finder should be reset
// after the given duration. When the sample reset duration is zero, the split
// finder should not be reset.
func TestDeciderSampleReset(t *testing.T) {
	defer leaktest.AfterTest(t)()

	rng := rand.New(rand.NewPCG(12, 12))
	loadSplitConfig := testLoadSplitConfig{
		randSource:          rng,
		useWeighted:         false,
		statRetention:       2 * time.Second,
		statThreshold:       1,
		sampleResetDuration: 10 * time.Second,
	}
	ctx := context.Background()
	tick := 0

	var d Decider
	Init(&d, &loadSplitConfig, newSplitterMetrics(), SplitQPS)

	require.Nil(t, d.mu.splitFinder)
	d.Record(ctx, ms(tick), ld(100), func() roachpb.Span {
		return roachpb.Span{Key: keys.SystemSQLCodec.TablePrefix(uint32(0))}
	})
	// The split finder should be created as the second sample is recorded and
	// the stat remains above the threshold (1) each tick.
	for i := 0; i < 10; i++ {
		tick += 1000
		d.Record(ctx, ms(tick), ld(100), func() roachpb.Span {
			return roachpb.Span{Key: keys.SystemSQLCodec.TablePrefix(uint32(0))}
		})
		require.NotNil(t, d.mu.splitFinder, (*lockedDecider)(&d))
	}

	// Tick one more time, now the sample reset duration (10s) has passed and the
	// split finder should be reset.
	tick += 1000
	d.Record(ctx, ms(tick), ld(100), func() roachpb.Span {
		return roachpb.Span{Key: keys.SystemSQLCodec.TablePrefix(uint32(0))}
	})
	require.Nil(t, d.mu.splitFinder, (*lockedDecider)(&d))

	// Immediately following the last tick where the splitFinder was reset, it
	// should be recreated as the stat is still above the threshold.
	for i := 0; i < 10; i++ {
		tick += 1000
		d.Record(ctx, ms(tick), ld(100), func() roachpb.Span {
			return roachpb.Span{Key: keys.SystemSQLCodec.TablePrefix(uint32(0))}
		})
		require.NotNil(t, d.mu.splitFinder, (*lockedDecider)(&d))
	}
	// Set the sample reset duration to 0, which should cause the split finder to
	// not be reset in the next tick, unlike before when the sample reset
	// duration was 10s.
	loadSplitConfig.sampleResetDuration = 0
	tick += 1000
	d.Record(ctx, ms(tick), ld(100), func() roachpb.Span {
		return roachpb.Span{Key: keys.SystemSQLCodec.TablePrefix(uint32(0))}
	})
	require.NotNil(t, d.mu.splitFinder, (*lockedDecider)(&d))
}
