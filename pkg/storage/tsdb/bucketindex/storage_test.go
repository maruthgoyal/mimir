// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/storage/tsdb/bucketindex/storage_test.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package bucketindex

import (
	"context"
	"path"
	"strings"
	"testing"

	"github.com/go-kit/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/mimir/pkg/storage/tsdb/block"
	mimir_testutil "github.com/grafana/mimir/pkg/storage/tsdb/testutil"
)

func TestReadIndex_ShouldReturnErrorIfIndexDoesNotExist(t *testing.T) {
	bkt, _ := mimir_testutil.PrepareFilesystemBucket(t)

	idx, err := ReadIndex(context.Background(), bkt, "user-1", nil, log.NewNopLogger())
	require.Equal(t, ErrIndexNotFound, err)
	require.Nil(t, idx)
}

func TestReadIndex_ShouldReturnErrorIfIndexIsCorrupted(t *testing.T) {
	const userID = "user-1"

	ctx := context.Background()
	bkt, _ := mimir_testutil.PrepareFilesystemBucket(t)

	// Write a corrupted index.
	require.NoError(t, bkt.Upload(ctx, path.Join(userID, IndexCompressedFilename), strings.NewReader("invalid!}")))

	idx, err := ReadIndex(ctx, bkt, userID, nil, log.NewNopLogger())
	require.Equal(t, ErrIndexCorrupted, err)
	require.Nil(t, idx)
}

func TestReadIndex_ShouldReturnTheParsedIndexOnSuccess(t *testing.T) {
	const userID = "user-1"

	ctx := context.Background()
	logger := log.NewNopLogger()

	bkt, _ := mimir_testutil.PrepareFilesystemBucket(t)

	// Mock some blocks in the storage.
	bkt = block.BucketWithGlobalMarkers(bkt)
	block.MockStorageBlock(t, bkt, userID, 10, 20)
	block.MockStorageBlock(t, bkt, userID, 20, 30)
	block.MockStorageDeletionMark(t, bkt, userID, block.MockStorageBlock(t, bkt, userID, 30, 40))

	// Write the index.
	u := NewUpdater(bkt, userID, nil, 16, 16, logger)
	expectedIdx, _, err := u.UpdateIndex(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, WriteIndex(ctx, bkt, userID, nil, expectedIdx))

	// Read it back and compare.
	actualIdx, err := ReadIndex(ctx, bkt, userID, nil, logger)
	require.NoError(t, err)
	assert.Equal(t, expectedIdx, actualIdx)
}

func BenchmarkReadIndex(b *testing.B) {
	const (
		numBlocks             = 1000
		numBlockDeletionMarks = 100
		userID                = "user-1"
	)

	ctx := context.Background()
	logger := log.NewNopLogger()

	bkt, _ := mimir_testutil.PrepareFilesystemBucket(b)

	// Mock some blocks and deletion marks in the storage.
	bkt = block.BucketWithGlobalMarkers(bkt)
	for i := 0; i < numBlocks; i++ {
		minT := int64(i * 10)
		maxT := int64((i + 1) * 10)

		meta := block.MockStorageBlock(b, bkt, userID, minT, maxT)

		if i < numBlockDeletionMarks {
			block.MockStorageDeletionMark(b, bkt, userID, meta)
		}
	}

	// Write the index.
	u := NewUpdater(bkt, userID, nil, 16, 16, logger)
	idx, _, err := u.UpdateIndex(ctx, nil)
	require.NoError(b, err)
	require.NoError(b, WriteIndex(ctx, bkt, userID, nil, idx))

	// Read it back once just to make sure the index contains the expected data.
	idx, err = ReadIndex(ctx, bkt, userID, nil, logger)
	require.NoError(b, err)
	require.Len(b, idx.Blocks, numBlocks)
	require.Len(b, idx.BlockDeletionMarks, numBlockDeletionMarks)

	b.ResetTimer()

	for n := 0; n < b.N; n++ {
		_, err := ReadIndex(ctx, bkt, userID, nil, logger)
		require.NoError(b, err)
	}
}

func TestDeleteIndex_ShouldNotReturnErrorIfIndexDoesNotExist(t *testing.T) {
	ctx := context.Background()
	bkt, _ := mimir_testutil.PrepareFilesystemBucket(t)

	assert.NoError(t, DeleteIndex(ctx, bkt, "user-1", nil))
}
