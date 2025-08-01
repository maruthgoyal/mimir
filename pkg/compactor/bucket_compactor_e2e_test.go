// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/thanos-io/thanos/blob/2be2db77/pkg/compact/compact_e2e_test.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Thanos Authors.

package compactor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/grafana/dskit/runutil"
	"github.com/oklog/ulid/v2"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/promslog"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/index"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thanos-io/objstore"
	"github.com/thanos-io/objstore/providers/filesystem"
	"golang.org/x/sync/errgroup"

	"github.com/grafana/mimir/pkg/storage/indexheader"
	"github.com/grafana/mimir/pkg/storage/tsdb/block"
	util_log "github.com/grafana/mimir/pkg/util/log"
)

func TestSyncer_GarbageCollect_e2e(t *testing.T) {
	foreachStore(t, func(t *testing.T, bkt objstore.Bucket) {
		// Use bucket with global markers to make sure that our custom filters work correctly.
		bkt = block.BucketWithGlobalMarkers(bkt)

		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		// Generate 10 source block metas and construct higher level blocks
		// that are higher compactions of them.
		var metas []*block.Meta
		var ids []ulid.ULID

		for i := 0; i < 10; i++ {
			var m block.Meta

			m.Version = 1
			m.ULID = ulid.MustNew(uint64(i), nil)
			m.Compaction.Sources = []ulid.ULID{m.ULID}
			m.Compaction.Level = 1
			m.MinTime = 0
			m.MaxTime = 2 * time.Hour.Milliseconds()

			ids = append(ids, m.ULID)
			metas = append(metas, &m)
		}

		var m1 block.Meta
		m1.Version = 1
		m1.ULID = ulid.MustNew(100, nil)
		m1.Compaction.Level = 2
		m1.Compaction.Sources = ids[:4]
		m1.Thanos.Downsample.Resolution = 0

		var m2 block.Meta
		m2.Version = 1
		m2.ULID = ulid.MustNew(200, nil)
		m2.Compaction.Level = 2
		m2.Compaction.Sources = ids[4:8] // last two source IDs is not part of a level 2 block.
		m2.Thanos.Downsample.Resolution = 0

		var m3 block.Meta
		m3.Version = 1
		m3.ULID = ulid.MustNew(300, nil)
		m3.Compaction.Level = 3
		m3.Compaction.Sources = ids[:9] // last source ID is not part of level 3 block.
		m3.Thanos.Downsample.Resolution = 0
		m3.MinTime = 0
		m3.MaxTime = 2 * time.Hour.Milliseconds()

		var m4 block.Meta
		m4.Version = 1
		m4.ULID = ulid.MustNew(400, nil)
		m4.Compaction.Level = 2
		m4.Compaction.Sources = ids[9:] // covers the last block but is a different resolution. Must not trigger deletion.
		m4.Thanos.Downsample.Resolution = 1000
		m4.MinTime = 0
		m4.MaxTime = 2 * time.Hour.Milliseconds()

		var m5 block.Meta
		m5.Version = 1
		m5.ULID = ulid.MustNew(500, nil)
		m5.Compaction.Level = 2
		m5.Compaction.Sources = ids[8:9] // built from block 8, but different resolution. Block 8 is already included in m3, can be deleted.
		m5.Thanos.Downsample.Resolution = 1000
		m5.MinTime = 0
		m5.MaxTime = 2 * time.Hour.Milliseconds()

		// Create all blocks in the bucket.
		for _, m := range append(metas, &m1, &m2, &m3, &m4, &m5) {
			fmt.Println("create", m.ULID)
			var buf bytes.Buffer
			require.NoError(t, json.NewEncoder(&buf).Encode(&m))
			require.NoError(t, bkt.Upload(ctx, path.Join(m.ULID.String(), block.MetaFilename), bytes.NewReader(buf.Bytes())))
		}

		duplicateBlocksFilter := NewShardAwareDeduplicateFilter()
		metaFetcher, err := block.NewMetaFetcher(nil, 32, objstore.WithNoopInstr(bkt), "", nil, []block.MetadataFilter{
			duplicateBlocksFilter,
		}, nil, 0)
		require.NoError(t, err)

		blocksMarkedForDeletion := promauto.With(nil).NewCounter(prometheus.CounterOpts{})
		sy, err := newMetaSyncer(nil, nil, bkt, metaFetcher, duplicateBlocksFilter, blocksMarkedForDeletion)
		require.NoError(t, err)

		// Do one initial synchronization with the bucket.
		require.NoError(t, sy.SyncMetas(ctx))
		require.NoError(t, sy.GarbageCollect(ctx))

		var rem []ulid.ULID
		err = bkt.Iter(ctx, "", func(n string) error {
			id, ok := block.IsBlockDir(n)
			if !ok {
				return nil
			}
			deletionMarkFile := path.Join(id.String(), block.DeletionMarkFilename)

			exists, err := bkt.Exists(ctx, deletionMarkFile)
			if err != nil {
				return err
			}
			if !exists {
				rem = append(rem, id)
			}
			return nil
		})
		require.NoError(t, err)

		slices.SortFunc(rem, func(a, b ulid.ULID) int {
			return a.Compare(b)
		})

		// Only the level 3 block, the last source block in both resolutions should be left.
		assert.Equal(t, []ulid.ULID{metas[9].ULID, m3.ULID, m4.ULID, m5.ULID}, rem)

		// After another sync the changes should also be reflected in the local groups.
		require.NoError(t, sy.SyncMetas(ctx))
		require.NoError(t, sy.GarbageCollect(ctx))

		// Only the level 3 block, the last source block in both resolutions should be left.
		grouper := NewSplitAndMergeGrouper("user-1", []int64{2 * time.Hour.Milliseconds()}, 0, 0, log.NewNopLogger())
		groups, err := grouper.Groups(sy.Metas())
		require.NoError(t, err)

		assert.Equal(t, "0@17241709254077376921-merge--0-7200000", groups[0].Key())
		assert.Equal(t, []ulid.ULID{metas[9].ULID, m3.ULID}, groups[0].IDs())

		assert.Equal(t, "1000@17241709254077376921-merge--0-7200000", groups[1].Key())
		assert.Equal(t, []ulid.ULID{m4.ULID, m5.ULID}, groups[1].IDs())
	})
}

func TestGroupCompactE2E(t *testing.T) {
	foreachStore(t, func(t *testing.T, bkt objstore.Bucket) {
		// Use bucket with global markers to make sure that our custom filters work correctly.
		bkt = block.BucketWithGlobalMarkers(bkt)

		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		// Create fresh, empty directory for actual test.
		dir := t.TempDir()

		// Start dir checker... we make sure that "dir" only contains group subdirectories during compaction,
		// and not any block directories. Dir checker stops when context is canceled, or on first error,
		// in which case error is logger and test is failed. (We cannot use Fatal or FailNow from a goroutine).
		go func() {
			for ctx.Err() == nil {
				fs, err := os.ReadDir(dir)
				if err != nil && !os.IsNotExist(err) {
					t.Log("error while listing directory", dir)
					t.Fail()
					return
				}

				for _, fi := range fs {
					// Suffix used by Prometheus LeveledCompactor when doing compaction.
					toCheck := strings.TrimSuffix(fi.Name(), ".tmp-for-creation")

					_, err := ulid.Parse(toCheck)
					if err == nil {
						t.Log("found block directory in main compaction directory", fi.Name())
						t.Fail()
						return
					}
				}

				select {
				case <-time.After(100 * time.Millisecond):
					continue
				case <-ctx.Done():
					return
				}
			}
		}()

		logger := log.NewLogfmtLogger(os.Stderr)

		reg := prometheus.NewRegistry()

		duplicateBlocksFilter := NewShardAwareDeduplicateFilter()
		noCompactMarkerFilter := NewNoCompactionMarkFilter(objstore.WithNoopInstr(bkt))
		metaFetcher, err := block.NewMetaFetcher(nil, 32, objstore.WithNoopInstr(bkt), "", nil, []block.MetadataFilter{
			duplicateBlocksFilter,
			noCompactMarkerFilter,
		}, nil, 0)
		require.NoError(t, err)

		blocksMarkedForDeletion := promauto.With(nil).NewCounter(prometheus.CounterOpts{})
		sy, err := newMetaSyncer(nil, nil, bkt, metaFetcher, duplicateBlocksFilter, blocksMarkedForDeletion)
		require.NoError(t, err)

		comp, err := tsdb.NewLeveledCompactor(ctx, reg, util_log.SlogFromGoKit(logger), []int64{1000, 3000}, nil, nil)
		require.NoError(t, err)

		planner := NewSplitAndMergePlanner([]int64{1000, 3000})
		grouper := NewSplitAndMergeGrouper("user-1", []int64{1000, 3000}, 0, 0, logger)
		metrics := NewBucketCompactorMetrics(blocksMarkedForDeletion, prometheus.NewPedanticRegistry())
		cfg := indexheader.Config{VerifyOnLoad: true}
		bComp, err := NewBucketCompactor(
			logger, sy, grouper, planner, comp, dir, bkt, 2, true, ownAllJobs, sortJobsByNewestBlocksFirst, 0, 4, metrics, true, 32, cfg, 8,
		)
		require.NoError(t, err)

		// Compaction on empty should not fail.
		require.NoError(t, bComp.Compact(ctx, 0), 0)
		assert.Equal(t, 0.0, promtest.ToFloat64(sy.metrics.blocksMarkedForDeletion))
		assert.Equal(t, 0.0, promtest.ToFloat64(sy.metrics.garbageCollectionFailures))
		assert.Equal(t, 0.0, promtest.ToFloat64(metrics.blocksMarkedForNoCompact.WithLabelValues(block.OutOfOrderChunksNoCompactReason)))
		assert.Equal(t, 0.0, promtest.ToFloat64(metrics.groupCompactions))
		assert.Equal(t, 0.0, promtest.ToFloat64(metrics.groupCompactionRunsStarted))
		assert.Equal(t, 0.0, promtest.ToFloat64(metrics.groupCompactionRunsCompleted))
		assert.Equal(t, 0.0, promtest.ToFloat64(metrics.groupCompactionRunsFailed))
		assert.Equal(t, 0.0, promtest.ToFloat64(metrics.blockUploadsStarted))

		_, err = os.Stat(dir)
		assert.True(t, os.IsNotExist(err), "dir %s should be remove after compaction.", dir)

		// Test label name with slash, regression: https://github.com/thanos-io/thanos/issues/1661.
		extLabels := labels.FromStrings("e1", "1/weird")
		extLabels2 := labels.FromStrings("e1", "1")
		extLabels3 := labels.FromStrings("e1", "histograms")
		metas := createAndUpload(t, bkt, []blockgenSpec{
			{
				numFloatSamples: 100, mint: 500, maxt: 1000, extLset: extLabels, res: 124,
				series: []labels.Labels{
					labels.FromStrings("a", "1"),
					labels.FromStrings("a", "2", "b", "2"),
					labels.FromStrings("a", "3"),
					labels.FromStrings("a", "4"),
				},
			},
			{
				numFloatSamples: 100, mint: 2000, maxt: 3000, extLset: extLabels, res: 124,
				series: []labels.Labels{
					labels.FromStrings("a", "3"),
					labels.FromStrings("a", "4"),
					labels.FromStrings("a", "5"),
					labels.FromStrings("a", "6"),
				},
			},
			// Mix order to make sure compactor is able to deduct min time / max time.
			// Currently TSDB does not produces empty blocks (see: https://github.com/prometheus/tsdb/pull/374). However before v2.7.0 it was
			// so we still want to mimick this case as close as possible.
			{
				mint: 1000, maxt: 2000, extLset: extLabels, res: 124,
				// Empty block.
			},
			// Due to TSDB compaction delay (not compacting fresh block), we need one more block to be pushed to trigger compaction.
			{
				numFloatSamples: 100, mint: 3000, maxt: 4000, extLset: extLabels, res: 124,
				series: []labels.Labels{
					labels.FromStrings("a", "7"),
				},
			},
			// Extra block for "distraction" for different resolution and one for different labels.
			{
				numFloatSamples: 100, mint: 5000, maxt: 6000, extLset: labels.FromStrings("e1", "2"), res: 124,
				series: []labels.Labels{
					labels.FromStrings("a", "7"),
				},
			},
			// Extra block for "distraction" for different resolution and one for different labels.
			{
				numFloatSamples: 100, mint: 4000, maxt: 5000, extLset: extLabels, res: 0,
				series: []labels.Labels{
					labels.FromStrings("a", "7"),
				},
			},
			// Second group (extLabels2).
			{
				numFloatSamples: 100, mint: 2000, maxt: 3000, extLset: extLabels2, res: 124,
				series: []labels.Labels{
					labels.FromStrings("a", "3"),
					labels.FromStrings("a", "4"),
					labels.FromStrings("a", "6"),
				},
			},
			{
				numFloatSamples: 100, mint: 0, maxt: 1000, extLset: extLabels2, res: 124,
				series: []labels.Labels{
					labels.FromStrings("a", "1"),
					labels.FromStrings("a", "2", "b", "2"),
					labels.FromStrings("a", "3"),
					labels.FromStrings("a", "4"),
				},
			},
			// Due to TSDB compaction delay (not compacting fresh block), we need one more block to be pushed to trigger compaction.
			{
				numFloatSamples: 100, mint: 3000, maxt: 4000, extLset: extLabels2, res: 124,
				series: []labels.Labels{
					labels.FromStrings("a", "7"),
				},
			},
			// Third group (extLabels3) for native histograms.
			{
				numHistogramSamples: 100, mint: 500, maxt: 1000, extLset: extLabels3, res: 124,
				series: []labels.Labels{
					labels.FromStrings("a", "1"),
					labels.FromStrings("a", "2", "b", "2"),
					labels.FromStrings("a", "3"),
					labels.FromStrings("a", "4"),
				},
			},
			{
				numHistogramSamples: 100, mint: 2000, maxt: 3000, extLset: extLabels3, res: 124,
				series: []labels.Labels{
					labels.FromStrings("a", "3"),
					labels.FromStrings("a", "4"),
					labels.FromStrings("a", "5"),
					labels.FromStrings("a", "6"),
				},
			},
			// Due to TSDB compaction delay (not compacting fresh block), we need one more block to be pushed to trigger compaction.
			{
				numFloatSamples: 100, mint: 3000, maxt: 4000, extLset: extLabels3, res: 124,
				series: []labels.Labels{
					labels.FromStrings("a", "7"),
				},
			},
		})

		require.NoError(t, bComp.Compact(ctx, 0), 0)
		assert.Equal(t, 7.0, promtest.ToFloat64(sy.metrics.blocksMarkedForDeletion))
		assert.Equal(t, 0.0, promtest.ToFloat64(metrics.blocksMarkedForNoCompact.WithLabelValues(block.OutOfOrderChunksNoCompactReason)))
		assert.Equal(t, 0.0, promtest.ToFloat64(sy.metrics.garbageCollectionFailures))
		assert.Equal(t, 3.0, promtest.ToFloat64(metrics.groupCompactions))
		assert.Equal(t, 3.0, promtest.ToFloat64(metrics.groupCompactionRunsStarted))
		assert.Equal(t, 3.0, promtest.ToFloat64(metrics.groupCompactionRunsCompleted))
		assert.Equal(t, 0.0, promtest.ToFloat64(metrics.groupCompactionRunsFailed))
		assert.Equal(t, 3.0, promtest.ToFloat64(metrics.blockUploadsStarted))

		_, err = os.Stat(dir)
		assert.True(t, os.IsNotExist(err), "dir %s should be remove after compaction.", dir)

		// Check object storage. All blocks that were included in new compacted one should be removed. New compacted ones
		// are present and looks as expected.
		nonCompactedExpected := map[ulid.ULID]bool{
			metas[3].ULID: false,
			metas[4].ULID: false,
			metas[5].ULID: false,
			metas[8].ULID: false,
		}
		others := map[string]block.Meta{}
		require.NoError(t, bkt.Iter(ctx, "", func(n string) error {
			id, ok := block.IsBlockDir(n)
			if !ok {
				return nil
			}

			if _, ok := nonCompactedExpected[id]; ok {
				nonCompactedExpected[id] = true
				return nil
			}

			meta, err := block.DownloadMeta(ctx, logger, bkt, id)
			if err != nil {
				return err
			}

			others[defaultGroupKey(meta.Thanos.Downsample.Resolution, labels.FromMap(meta.Thanos.Labels))] = meta
			return nil
		}))

		// expect the blocks that are compacted to have sparse-index-headers in object storage.
		require.NoError(t, bkt.Iter(ctx, "", func(n string) error {
			id, ok := block.IsBlockDir(n)
			if !ok {
				return nil
			}

			if _, ok := others[id.String()]; ok {
				p := path.Join(id.String(), block.SparseIndexHeaderFilename)
				exists, _ := bkt.Exists(ctx, p)
				assert.True(t, exists, "expected sparse index headers not found %s", p)
			}
			return nil
		}))

		for id, found := range nonCompactedExpected {
			assert.True(t, found, "not found expected block %s", id.String())
		}

		// We expect three compacted blocks only outside of what we expected in `nonCompactedExpected`.
		assert.Equal(t, 3, len(others))
		{
			meta, ok := others[defaultGroupKey(124, extLabels)]
			assert.True(t, ok, "meta not found")

			assert.Equal(t, int64(500), meta.MinTime)
			assert.Equal(t, int64(3000), meta.MaxTime)
			assert.Equal(t, uint64(6), meta.Stats.NumSeries)
			assert.Equal(t, uint64(2*4*100), meta.Stats.NumSamples)      // Only 2 times 4*100 because one block was empty.
			assert.Equal(t, uint64(2*4*100), meta.Stats.NumFloatSamples) // Only float samples.
			assert.Equal(t, uint64(0), meta.Stats.NumHistogramSamples)   // Only float samples.
			assert.Equal(t, 2, meta.Compaction.Level)
			assert.Equal(t, []ulid.ULID{metas[0].ULID, metas[1].ULID, metas[2].ULID}, meta.Compaction.Sources)

			// Check thanos meta.
			assert.True(t, labels.Equal(extLabels, labels.FromMap(meta.Thanos.Labels)), "ext labels does not match")
			assert.Equal(t, int64(124), meta.Thanos.Downsample.Resolution)
			assert.True(t, len(meta.Thanos.SegmentFiles) > 0, "compacted blocks have segment files set")
		}
		{
			meta, ok := others[defaultGroupKey(124, extLabels2)]
			assert.True(t, ok, "meta not found")

			assert.Equal(t, int64(0), meta.MinTime)
			assert.Equal(t, int64(3000), meta.MaxTime)
			assert.Equal(t, uint64(5), meta.Stats.NumSeries)
			assert.Equal(t, uint64(2*4*100-100), meta.Stats.NumSamples)
			assert.Equal(t, uint64(2*4*100-100), meta.Stats.NumFloatSamples)
			assert.Equal(t, uint64(0), meta.Stats.NumHistogramSamples)
			assert.Equal(t, 2, meta.Compaction.Level)
			assert.Equal(t, []ulid.ULID{metas[6].ULID, metas[7].ULID}, meta.Compaction.Sources)

			// Check thanos meta.
			assert.True(t, labels.Equal(extLabels2, labels.FromMap(meta.Thanos.Labels)), "ext labels does not match")
			assert.Equal(t, int64(124), meta.Thanos.Downsample.Resolution)
			assert.True(t, len(meta.Thanos.SegmentFiles) > 0, "compacted blocks have segment files set")
		}
		{
			meta, ok := others[defaultGroupKey(124, extLabels3)]
			assert.True(t, ok, "meta not found")

			assert.Equal(t, int64(500), meta.MinTime)
			assert.Equal(t, int64(3000), meta.MaxTime)
			assert.Equal(t, uint64(6), meta.Stats.NumSeries)
			assert.Equal(t, uint64(2*4*100), meta.Stats.NumSamples)
			assert.Equal(t, uint64(0), meta.Stats.NumFloatSamples)
			assert.Equal(t, uint64(2*4*100), meta.Stats.NumHistogramSamples) // Only histogram samples.
			assert.Equal(t, 2, meta.Compaction.Level)
			assert.Equal(t, []ulid.ULID{metas[9].ULID, metas[10].ULID}, meta.Compaction.Sources)

			// Check thanos meta.
			assert.True(t, labels.Equal(extLabels3, labels.FromMap(meta.Thanos.Labels)), "ext labels does not match")
			assert.Equal(t, int64(124), meta.Thanos.Downsample.Resolution)
			assert.True(t, len(meta.Thanos.SegmentFiles) > 0, "compacted blocks have segment files set")

		}
	})
}

type blockgenSpec struct {
	mint, maxt          int64
	series              []labels.Labels
	numFloatSamples     int
	numHistogramSamples int
	extLset             labels.Labels
	res                 int64
}

func createAndUpload(t testing.TB, bkt objstore.Bucket, blocks []blockgenSpec) (metas []*block.Meta) {
	prepareDir := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, b := range blocks {
		id, meta := createBlock(ctx, t, prepareDir, b)
		metas = append(metas, meta)
		require.NoError(t, block.Upload(ctx, log.NewNopLogger(), bkt, filepath.Join(prepareDir, id.String()), nil))
	}

	return metas
}

func createBlock(ctx context.Context, t testing.TB, prepareDir string, b blockgenSpec) (id ulid.ULID, meta *block.Meta) {
	var err error
	if b.numFloatSamples == 0 && b.numHistogramSamples == 0 {
		id, err = createEmptyBlock(prepareDir, b.mint, b.maxt, b.extLset, b.res)
	} else {
		id, err = createBlockWithOptions(ctx, prepareDir, b.series, b.numFloatSamples, b.numHistogramSamples, b.mint, b.maxt, b.extLset, b.res, false)
	}
	require.NoError(t, err)

	meta, err = block.ReadMetaFromDir(filepath.Join(prepareDir, id.String()))
	require.NoError(t, err)
	return
}

// Regression test for Thanos issue #2459.
func TestGarbageCollectDoesntCreateEmptyBlocksWithDeletionMarksOnly(t *testing.T) {
	logger := log.NewLogfmtLogger(os.Stderr)

	foreachStore(t, func(t *testing.T, bkt objstore.Bucket) {
		// Use bucket with global markers to make sure that our custom filters work correctly.
		bkt = block.BucketWithGlobalMarkers(bkt)

		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		// Generate two blocks, and then another block that covers both of them.
		var metas []*block.Meta
		var ids []ulid.ULID

		for i := 0; i < 2; i++ {
			var m block.Meta

			m.Version = 1
			m.ULID = ulid.MustNew(uint64(i), nil)
			m.Compaction.Sources = []ulid.ULID{m.ULID}
			m.Compaction.Level = 1

			ids = append(ids, m.ULID)
			metas = append(metas, &m)
		}

		var m1 block.Meta
		m1.Version = 1
		m1.ULID = ulid.MustNew(100, nil)
		m1.Compaction.Level = 2
		m1.Compaction.Sources = ids
		m1.Thanos.Downsample.Resolution = 0

		// Create all blocks in the bucket.
		for _, m := range append(metas, &m1) {
			fmt.Println("create", m.ULID)
			var buf bytes.Buffer
			require.NoError(t, json.NewEncoder(&buf).Encode(&m))
			require.NoError(t, bkt.Upload(ctx, path.Join(m.ULID.String(), block.MetaFilename), bytes.NewReader(buf.Bytes())))
		}

		blocksMarkedForDeletion := promauto.With(nil).NewCounter(prometheus.CounterOpts{})

		duplicateBlocksFilter := NewShardAwareDeduplicateFilter()
		metaFetcher, err := block.NewMetaFetcher(nil, 32, objstore.WithNoopInstr(bkt), "", nil, []block.MetadataFilter{
			duplicateBlocksFilter,
		}, nil, 0)
		require.NoError(t, err)

		sy, err := newMetaSyncer(nil, nil, bkt, metaFetcher, duplicateBlocksFilter, blocksMarkedForDeletion)
		require.NoError(t, err)

		// Do one initial synchronization with the bucket.
		require.NoError(t, sy.SyncMetas(ctx))
		require.NoError(t, sy.GarbageCollect(ctx))

		rem, err := listBlocksMarkedForDeletion(ctx, bkt)
		require.NoError(t, err)

		slices.SortFunc(rem, func(a, b ulid.ULID) int {
			return a.Compare(b)
		})

		assert.Equal(t, ids, rem)

		// Delete source blocks.
		for _, id := range ids {
			require.NoError(t, block.Delete(ctx, logger, bkt, id))
		}

		// After another garbage-collect, we should not find new blocks that are deleted with new deletion mark files.
		require.NoError(t, sy.SyncMetas(ctx))
		require.NoError(t, sy.GarbageCollect(ctx))

		rem, err = listBlocksMarkedForDeletion(ctx, bkt)
		require.NoError(t, err)
		assert.Equal(t, 0, len(rem))
	})
}

func listBlocksMarkedForDeletion(ctx context.Context, bkt objstore.Bucket) ([]ulid.ULID, error) {
	var rem []ulid.ULID
	err := bkt.Iter(ctx, "", func(n string) error {
		id, ok := block.IsBlockDir(n)
		if !ok {
			return nil
		}
		deletionMarkFile := path.Join(id.String(), block.DeletionMarkFilename)

		exists, err := bkt.Exists(ctx, deletionMarkFile)
		if err != nil {
			return err
		}
		if exists {
			rem = append(rem, id)
		}
		return nil
	})
	return rem, err
}

func foreachStore(t *testing.T, testFn func(t *testing.T, bkt objstore.Bucket)) {
	t.Parallel()

	// Mandatory Inmem. Not parallel, to detect problem early.
	if ok := t.Run("inmem", func(t *testing.T) {
		testFn(t, objstore.NewInMemBucket())
	}); !ok {
		return
	}

	// Mandatory Filesystem.
	t.Run("filesystem", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		b, err := filesystem.NewBucket(dir)
		require.NoError(t, err)
		testFn(t, b)
	})
}

// createEmptyBlock produces empty block like it was the case before fix: https://github.com/prometheus/tsdb/pull/374.
// (Prometheus pre v2.7.0).
func createEmptyBlock(dir string, mint, maxt int64, extLset labels.Labels, resolution int64) (ulid.ULID, error) {
	entropy := rand.New(rand.NewSource(time.Now().UnixNano()))
	uid := ulid.MustNew(ulid.Now(), entropy)

	if err := os.Mkdir(path.Join(dir, uid.String()), os.ModePerm); err != nil {
		return ulid.ULID{}, errors.Wrap(err, "close index")
	}

	if err := os.Mkdir(path.Join(dir, uid.String(), "chunks"), os.ModePerm); err != nil {
		return ulid.ULID{}, errors.Wrap(err, "close index")
	}

	w, err := index.NewWriter(context.Background(), path.Join(dir, uid.String(), "index"))
	if err != nil {
		return ulid.ULID{}, errors.Wrap(err, "new index")
	}

	if err := w.Close(); err != nil {
		return ulid.ULID{}, errors.Wrap(err, "close index")
	}

	m := tsdb.BlockMeta{
		Version: 1,
		ULID:    uid,
		MinTime: mint,
		MaxTime: maxt,
		Compaction: tsdb.BlockMetaCompaction{
			Level:   1,
			Sources: []ulid.ULID{uid},
		},
	}
	b, err := json.Marshal(&m)
	if err != nil {
		return ulid.ULID{}, err
	}

	if err := os.WriteFile(path.Join(dir, uid.String(), "meta.json"), b, os.ModePerm); err != nil {
		return ulid.ULID{}, errors.Wrap(err, "saving meta.json")
	}

	if _, err = block.InjectThanosMeta(log.NewNopLogger(), filepath.Join(dir, uid.String()), block.ThanosMeta{
		Labels:     extLset.Map(),
		Downsample: block.ThanosDownsample{Resolution: resolution},
		Source:     block.TestSource,
	}, nil); err != nil {
		return ulid.ULID{}, errors.Wrap(err, "finalize block")
	}

	return uid, nil
}

func createBlockWithOptions(
	ctx context.Context,
	dir string,
	series []labels.Labels,
	numFloatSamples int,
	numHistogramSamples int,
	mint, maxt int64,
	extLset labels.Labels,
	resolution int64,
	tombstones bool,
) (id ulid.ULID, err error) {
	if numFloatSamples > 0 && numHistogramSamples > 0 {
		return id, errors.New("not creating block with both float and histogram samples")
	}
	numSamples := numFloatSamples + numHistogramSamples

	headOpts := tsdb.DefaultHeadOptions()
	headOpts.EnableNativeHistograms.Store(true)
	headOpts.ChunkDirRoot = filepath.Join(dir, "chunks")
	headOpts.ChunkRange = 10000000000
	h, err := tsdb.NewHead(nil, nil, nil, nil, headOpts, nil)
	if err != nil {
		return id, errors.Wrap(err, "create head block")
	}
	defer func() {
		runutil.CloseWithErrCapture(&err, h, "TSDB Head")
		if e := os.RemoveAll(headOpts.ChunkDirRoot); e != nil {
			err = errors.Wrap(e, "delete chunks dir")
		}
	}()

	var g errgroup.Group
	var timeStepSize = (maxt - mint) / int64(numSamples+1)
	var batchSize = len(series) / runtime.GOMAXPROCS(0)

	for len(series) > 0 {
		l := batchSize
		if len(series) < 1000 {
			l = len(series)
		}
		batch := series[:l]
		series = series[l:]

		g.Go(func() error {
			t := mint

			for i := 0; i < numSamples; i++ {
				app := h.Appender(ctx)

				for _, lset := range batch {
					var err error
					if numFloatSamples > 0 {
						_, err = app.Append(0, lset, t, rand.Float64())
					} else {
						count := rand.Int63()
						// Append a minimal histogram with a single bucket.
						_, err = app.AppendHistogram(0, lset, t, &histogram.Histogram{
							Count:           uint64(count),
							PositiveSpans:   []histogram.Span{{Offset: 0, Length: 1}},
							PositiveBuckets: []int64{count},
						}, nil)
					}
					if err != nil {
						if rerr := app.Rollback(); rerr != nil {
							err = errors.Wrapf(err, "rollback failed: %v", rerr)
						}

						return errors.Wrap(err, "add sample")
					}
				}
				if err := app.Commit(); err != nil {
					return errors.Wrap(err, "commit")
				}
				t += timeStepSize
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return id, err
	}
	c, err := tsdb.NewLeveledCompactor(ctx, nil, promslog.NewNopLogger(), []int64{maxt - mint}, nil, nil)
	if err != nil {
		return id, errors.Wrap(err, "create compactor")
	}

	blocks, err := c.Write(dir, h, mint, maxt, nil)
	if err != nil {
		return id, errors.Wrap(err, "write block")
	}

	if len(blocks) == 0 || (blocks[0] == ulid.ULID{}) {
		return id, errors.Errorf("nothing to write, asked for %d samples", numSamples)
	}
	if len(blocks) > 1 {
		return id, errors.Errorf("expected one block, got %d, asked for %d samples", len(blocks), numSamples)
	}

	id = blocks[0]

	blockDir := filepath.Join(dir, id.String())

	if _, err = block.InjectThanosMeta(log.NewNopLogger(), blockDir, block.ThanosMeta{
		Labels:     extLset.Map(),
		Downsample: block.ThanosDownsample{Resolution: resolution},
		Source:     block.TestSource,
		Files:      []block.File{},
	}, nil); err != nil {
		return id, errors.Wrap(err, "finalize block")
	}

	if !tombstones {
		if err = os.Remove(filepath.Join(dir, id.String(), "tombstones")); err != nil {
			return id, errors.Wrap(err, "remove tombstones")
		}
	}

	return id, nil
}
