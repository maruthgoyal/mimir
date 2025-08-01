// SPDX-License-Identifier: AGPL-3.0-only

package compactor

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/oklog/ulid/v2"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/grafana/mimir/pkg/storage/sharding"
	mimir_tsdb "github.com/grafana/mimir/pkg/storage/tsdb"
	"github.com/grafana/mimir/pkg/storage/tsdb/block"
)

type SplitAndMergeGrouper struct {
	userID string
	ranges []int64
	logger log.Logger

	// Number of shards to split source blocks into.
	shardCount uint32

	// Number of groups that blocks used for splitting are grouped into.
	splitGroupsCount uint32
}

// NewSplitAndMergeGrouper makes a new SplitAndMergeGrouper. The provided ranges must be sorted.
// If shardCount is 0, the splitting stage is disabled.
func NewSplitAndMergeGrouper(
	userID string,
	ranges []int64,
	shardCount uint32,
	splitGroupsCount uint32,
	logger log.Logger,
) *SplitAndMergeGrouper {
	return &SplitAndMergeGrouper{
		userID:           userID,
		ranges:           ranges,
		shardCount:       shardCount,
		splitGroupsCount: splitGroupsCount,
		logger:           logger,
	}
}

func (g *SplitAndMergeGrouper) Groups(blocks map[ulid.ULID]*block.Meta) (res []*Job, err error) {
	flatBlocks := make([]*block.Meta, 0, len(blocks))
	for _, b := range blocks {
		flatBlocks = append(flatBlocks, b)
	}

	for _, job := range planCompaction(g.userID, flatBlocks, g.ranges, g.shardCount, g.splitGroupsCount) {
		// Sanity check: if splitting is disabled, we don't expect any job for the split stage.
		if g.shardCount <= 0 && job.stage == stageSplit {
			return nil, errors.Errorf("unexpected split stage job because splitting is disabled: %s", job.String())
		}

		// The group key is used by the compactor as a unique identifier of the compaction job.
		// Its content is not important for the compactor, but uniqueness must be guaranteed.
		groupKey := fmt.Sprintf("%s-%s-%s-%d-%d",
			defaultGroupKeyWithoutShardID(job.blocks[0].Thanos),
			job.stage,
			job.shardID,
			job.rangeStart,
			job.rangeEnd)

		// All the blocks within the same group have the same downsample
		// resolution and external labels.
		resolution := job.blocks[0].Thanos.Downsample.Resolution
		externalLabels := labels.FromMap(job.blocks[0].Thanos.Labels)

		compactionJob := newJob(
			g.userID,
			groupKey,
			externalLabels,
			resolution,
			job.stage == stageSplit,
			g.shardCount,
			job.shardingKey(),
		)

		for _, m := range job.blocks {
			if err := compactionJob.AppendMeta(m); err != nil {
				return nil, errors.Wrap(err, "add block to compaction group")
			}
		}

		res = append(res, compactionJob)
		level.Debug(g.logger).Log("msg", "grouper found a compactable blocks group", "groupKey", groupKey, "job", job.String())
	}

	return res, nil
}

// planCompaction analyzes the input blocks and returns a list of compaction jobs that can be
// run concurrently. Each returned job may belong either to this compactor instance or another one
// in the cluster, so the caller should check if they belong to their instance before running them.
func planCompaction(userID string, blocks []*block.Meta, ranges []int64, shardCount, splitGroups uint32) (jobs []*job) {
	if len(blocks) == 0 || len(ranges) == 0 {
		return nil
	}

	// First of all we have to group blocks using the default grouping, but not
	// considering the shard ID in the external labels (because it will be checked later).
	mainGroups := map[string][]*block.Meta{}
	for _, b := range blocks {
		key := defaultGroupKeyWithoutShardID(b.Thanos)
		mainGroups[key] = append(mainGroups[key], b)
	}

	for _, mainBlocks := range mainGroups {
		// Sort blocks by min time.
		sortMetasByMinTime(mainBlocks)

		for _, tr := range ranges {
		nextJob:
			for _, job := range planCompactionByRange(userID, mainBlocks, tr, tr == ranges[0], shardCount, splitGroups) {
				// We can plan a job only if it doesn't conflict with other jobs already planned.
				// Since we run the planning for each compaction range in increasing order, we guarantee
				// that a job for the current time range is planned only if there's no other job for the
				// same shard ID and an overlapping smaller time range.
				for _, j := range jobs {
					if job.conflicts(j) {
						continue nextJob
					}
				}

				jobs = append(jobs, job)
			}
		}
	}

	// Ensure we don't compact the most recent blocks prematurely. We allow a job to remain if:
	// - its range is before the most recent block
	// - its range is at least 1 job length in the past
	// - its max compaction level is 1
	// - it fully covers the range
	highestMaxTime := getMaxTime(blocks)

	for idx := 0; idx < len(jobs); {
		job := jobs[idx]

		// If the job covers a range before the most recent block, it's fine.
		if job.rangeEnd <= highestMaxTime {
			idx++
			continue
		}

		// If the job covers a range at least 1 job length in the past, it's fine.
		if job.rangeEnd+job.rangeLength() <= time.Now().UnixMilli() {
			idx++
			continue
		}

		// If the job only contains level 1 blocks, it's fine.
		if job.maxCompactionLevel() == 1 {
			idx++
			continue
		}

		// If the job covers the full range, it's fine.
		if job.maxTime()-job.minTime() == job.rangeLength() {
			idx++
			continue
		}

		// We have found a job which would compact recent blocks prematurely,
		// so we need to filter it out.
		jobs = append(jobs[:idx], jobs[idx+1:]...)
	}

	// Jobs will be sorted later using configured job sorting algorithm.
	// Here we sort them by sharding key, to keep the output stable for testing.
	slices.SortStableFunc(jobs, func(a, b *job) int {
		aKey, bKey := a.shardingKey(), b.shardingKey()
		if aKey != bKey {
			return strings.Compare(aKey, bKey)
		}

		// The sharding key could be equal but external labels can still be different.
		aGroupKey := defaultGroupKeyWithoutShardID(a.blocks[0].Thanos)
		bGroupKey := defaultGroupKeyWithoutShardID(b.blocks[0].Thanos)
		return strings.Compare(aGroupKey, bGroupKey)
	})

	return jobs
}

// planCompactionByRange analyzes the input blocks and returns a list of compaction jobs to
// compact blocks for the given compaction time range. Input blocks MUST be sorted by MinTime.
func planCompactionByRange(userID string, blocks []*block.Meta, tr int64, isSmallestRange bool, shardCount, splitGroups uint32) (jobs []*job) {
	groups := groupBlocksByRange(blocks, tr)

	for _, group := range groups {
		// If this is the smallest time range and there's any non-split block,
		// then we should plan a job to split blocks.
		if shardCount > 0 && isSmallestRange {
			if splitJobs := planSplitting(userID, group, splitGroups); len(splitJobs) > 0 {
				jobs = append(jobs, splitJobs...)
				continue
			}
		}

		// If we reach this point, all blocks for this time range have already been split
		// (or we're not processing the smallest time range, or splitting is disabled).
		// Then, we can check if there's any group of blocks to be merged together for each shard.
		for shardID, shardBlocks := range groupBlocksByShardID(group.blocks) {
			// No merging to do if there are less than 2 blocks.
			if len(shardBlocks) < 2 {
				continue
			}

			jobs = append(jobs, &job{
				userID:  userID,
				stage:   stageMerge,
				shardID: shardID,
				blocksGroup: blocksGroup{
					rangeStart: group.rangeStart,
					rangeEnd:   group.rangeEnd,
					blocks:     shardBlocks,
				},
			})
		}
	}

	return jobs
}

// planSplitting returns a job to split the blocks in the input group or nil if there's nothing to do because
// all blocks in the group have already been split.
func planSplitting(userID string, group blocksGroup, splitGroups uint32) []*job {
	blocks := group.getNonShardedBlocks()
	if len(blocks) == 0 {
		return nil
	}

	jobs := map[uint32]*job{}

	if splitGroups == 0 {
		splitGroups = 1
	}

	// The number of source blocks could be very large so, to have a better horizontal scaling, we should group
	// the source blocks into N groups (where N = number of shards) and create a job for each group of blocks to
	// merge and split.
	for _, block := range blocks {
		splitGroup := mimir_tsdb.HashBlockID(block.ULID) % splitGroups

		if jobs[splitGroup] == nil {
			jobs[splitGroup] = &job{
				userID:  userID,
				stage:   stageSplit,
				shardID: sharding.FormatShardIDLabelValue(uint64(splitGroup), uint64(splitGroups)),
				blocksGroup: blocksGroup{
					rangeStart: group.rangeStart,
					rangeEnd:   group.rangeEnd,
				},
			}
		}

		jobs[splitGroup].blocks = append(jobs[splitGroup].blocks, block)
	}

	// Convert the output.
	out := make([]*job, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, job)
	}

	return out
}

// groupBlocksByShardID groups the blocks by shard ID (read from the block external labels).
// If a block doesn't have any shard ID in the external labels, it will be grouped with the
// shard ID set to an empty string.
func groupBlocksByShardID(blocks []*block.Meta) map[string][]*block.Meta {
	groups := map[string][]*block.Meta{}

	for _, block := range blocks {
		// If the label doesn't exist, we'll group together such blocks using an
		// empty string as shard ID.
		shardID := block.Thanos.Labels[mimir_tsdb.CompactorShardIDExternalLabel]
		groups[shardID] = append(groups[shardID], block)
	}

	return groups
}

// groupBlocksByRange groups the blocks by the time range. The range sequence starts at 0.
// Input blocks MUST be sorted by MinTime.
//
// For example, if we have blocks [0-10, 10-20, 50-60, 90-100] and the split range tr is 30
// it returns [0-10, 10-20], [50-60], [90-100].
func groupBlocksByRange(blocks []*block.Meta, tr int64) []blocksGroup {
	var ret []blocksGroup

	for i := 0; i < len(blocks); {
		var (
			group blocksGroup
			m     = blocks[i]
		)

		group.rangeStart = getRangeStart(m, tr)
		group.rangeEnd = group.rangeStart + tr

		// Skip blocks that don't fall into the range. This can happen via mis-alignment or
		// by being the multiple of the intended range.
		if m.MaxTime > group.rangeEnd {
			i++
			continue
		}

		// Add all blocks to the current group that are within [t0, t0+tr].
		for ; i < len(blocks); i++ {
			// If the block does not start within this group, then we should break the iteration
			// and move it to the next group.
			if blocks[i].MinTime >= group.rangeEnd {
				break
			}

			// If the block doesn't fall into this group, but it started within this group then it
			// means it spans across multiple ranges and we should skip it.
			if blocks[i].MaxTime > group.rangeEnd {
				continue
			}

			group.blocks = append(group.blocks, blocks[i])
		}

		if len(group.blocks) > 0 {
			ret = append(ret, group)
		}
	}

	return ret
}

func getRangeStart(m *block.Meta, tr int64) int64 {
	// Compute start of aligned time range of size tr closest to the current block's start.
	// This code has been copied from TSDB.
	if m.MinTime >= 0 {
		return tr * (m.MinTime / tr)
	}
	return tr * ((m.MinTime - tr + 1) / tr)
}

func sortMetasByMinTime(metas []*block.Meta) []*block.Meta {
	slices.SortFunc(metas, func(a, b *block.Meta) int {
		if a.MinTime != b.MinTime {
			return cmp.Compare(a.MinTime, b.MinTime)
		}

		// Compare labels in case of same MinTime to get stable results.
		return labels.Compare(labels.FromMap(a.Thanos.Labels), labels.FromMap(b.Thanos.Labels))
	})

	return metas
}

// getMaxTime returns the highest max time across all input blocks.
func getMaxTime(blocks []*block.Meta) int64 {
	maxTime := int64(math.MinInt64)

	for _, block := range blocks {
		if block.MaxTime > maxTime {
			maxTime = block.MaxTime
		}
	}

	return maxTime
}

// defaultGroupKeyWithoutShardID returns the default group key excluding ShardIDLabelName
// when computing it.
func defaultGroupKeyWithoutShardID(meta block.ThanosMeta) string {
	return defaultGroupKey(meta.Downsample.Resolution, labelsWithoutShard(meta.Labels))
}

// Return labels built from base, but without any label with name equal to mimir_tsdb.CompactorShardIDExternalLabel.
func labelsWithoutShard(base map[string]string) labels.Labels {
	b := labels.NewScratchBuilder(len(base))
	for k, v := range base {
		if k != mimir_tsdb.CompactorShardIDExternalLabel {
			b.Add(k, v)
		}
	}
	b.Sort()
	return b.Labels()
}
