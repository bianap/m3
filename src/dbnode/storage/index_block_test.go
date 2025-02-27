// Copyright (c) 2018 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package storage

import (
	stdlibctx "context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/m3db/m3/src/dbnode/namespace"
	"github.com/m3db/m3/src/dbnode/storage/bootstrap/result"
	"github.com/m3db/m3/src/dbnode/storage/index"
	"github.com/m3db/m3/src/m3ninx/doc"
	"github.com/m3db/m3/src/m3ninx/idx"
	"github.com/m3db/m3/src/m3ninx/index/segment"
	"github.com/m3db/m3/src/x/context"
	"github.com/m3db/m3/src/x/ident"
	xtest "github.com/m3db/m3/src/x/test"
	xtime "github.com/m3db/m3/src/x/time"

	"github.com/golang/mock/gomock"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/mocktracer"
	"github.com/stretchr/testify/require"
)

var (
	namespaceIndexOptions = namespace.NewIndexOptions()

	defaultQuery = index.Query{
		Query: idx.NewTermQuery([]byte("foo"), []byte("bar")),
	}
)

type testWriteBatchOption func(index.WriteBatchOptions) index.WriteBatchOptions

func testWriteBatchBlockSizeOption(blockSize time.Duration) testWriteBatchOption {
	return func(o index.WriteBatchOptions) index.WriteBatchOptions {
		o.IndexBlockSize = blockSize
		return o
	}
}

func testWriteBatch(
	e index.WriteBatchEntry,
	d doc.Document,
	opts ...testWriteBatchOption,
) *index.WriteBatch {
	var options index.WriteBatchOptions
	for _, opt := range opts {
		options = opt(options)
	}
	b := index.NewWriteBatch(options)
	b.Append(e, d)
	return b
}

func testWriteBatchEntry(
	id ident.ID,
	tags ident.Tags,
	timestamp time.Time,
	fns index.OnIndexSeries,
) (index.WriteBatchEntry, doc.Document) {
	d := doc.Document{ID: copyBytes(id.Bytes())}
	for _, tag := range tags.Values() {
		d.Fields = append(d.Fields, doc.Field{
			Name:  copyBytes(tag.Name.Bytes()),
			Value: copyBytes(tag.Value.Bytes()),
		})
	}
	return index.WriteBatchEntry{
		Timestamp:     timestamp,
		OnIndexSeries: fns,
	}, d
}

func copyBytes(b []byte) []byte {
	return append([]byte(nil), b...)
}

func testNamespaceMetadata(blockSize, period time.Duration) namespace.Metadata {
	nopts := namespaceOptions.
		SetRetentionOptions(namespaceOptions.RetentionOptions().
			SetRetentionPeriod(period)).
		SetIndexOptions(
			namespaceIndexOptions.
				SetBlockSize(blockSize))
	md, err := namespace.NewMetadata(ident.StringID("testns"), nopts)
	if err != nil {
		panic(err)
	}
	return md
}

func TestNamespaceIndexNewBlockFn(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	blockSize := time.Hour
	now := time.Now().Truncate(blockSize).Add(2 * time.Minute)
	nowFn := func() time.Time { return now }
	opts := DefaultTestOptions()
	opts = opts.SetClockOptions(opts.ClockOptions().SetNowFn(nowFn))

	mockBlock := index.NewMockBlock(ctrl)
	mockBlock.EXPECT().Stats(gomock.Any()).Return(nil).AnyTimes()
	mockBlock.EXPECT().Close().Return(nil)
	newBlockFn := func(
		ts time.Time,
		md namespace.Metadata,
		_ index.BlockOptions,
		io index.Options,
	) (index.Block, error) {
		require.Equal(t, now.Truncate(blockSize), ts)
		return mockBlock, nil
	}
	md := testNamespaceMetadata(blockSize, 4*time.Hour)
	index, err := newNamespaceIndexWithNewBlockFn(md, newBlockFn, opts)
	require.NoError(t, err)

	defer func() {
		require.NoError(t, index.Close())
	}()

	blocksSlice := index.(*nsIndex).state.blockStartsDescOrder

	require.Equal(t, 1, len(blocksSlice))
	require.Equal(t, xtime.ToUnixNano(now.Truncate(blockSize)), blocksSlice[0])

	require.Equal(t, mockBlock, index.(*nsIndex).state.latestBlock)

	blocksMap := index.(*nsIndex).state.blocksByTime
	require.Equal(t, 1, len(blocksMap))
	blk, ok := blocksMap[xtime.ToUnixNano(now.Truncate(blockSize))]
	require.True(t, ok)
	require.Equal(t, mockBlock, blk)
}

func TestNamespaceIndexNewBlockFnRandomErr(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	blockSize := time.Hour
	now := time.Now().Truncate(blockSize).Add(2 * time.Minute)
	nowFn := func() time.Time { return now }
	opts := DefaultTestOptions()
	opts = opts.SetClockOptions(opts.ClockOptions().SetNowFn(nowFn))

	newBlockFn := func(
		ts time.Time,
		md namespace.Metadata,
		_ index.BlockOptions,
		io index.Options,
	) (index.Block, error) {
		return nil, fmt.Errorf("randomerr")
	}
	md := testNamespaceMetadata(blockSize, 4*time.Hour)
	_, err := newNamespaceIndexWithNewBlockFn(md, newBlockFn, opts)
	require.Error(t, err)
}

func TestNamespaceIndexWrite(t *testing.T) {
	ctrl := gomock.NewController(xtest.Reporter{T: t})
	defer ctrl.Finish()

	blockSize := time.Hour
	now := time.Now().Truncate(blockSize).Add(2 * time.Minute)
	nowFn := func() time.Time { return now }
	opts := DefaultTestOptions()
	opts = opts.SetClockOptions(opts.ClockOptions().SetNowFn(nowFn))

	mockBlock := index.NewMockBlock(ctrl)
	mockBlock.EXPECT().Stats(gomock.Any()).Return(nil).AnyTimes()
	mockBlock.EXPECT().Close().Return(nil)
	mockBlock.EXPECT().StartTime().Return(now.Truncate(blockSize)).AnyTimes()
	newBlockFn := func(
		ts time.Time,
		md namespace.Metadata,
		_ index.BlockOptions,
		io index.Options,
	) (index.Block, error) {
		require.Equal(t, now.Truncate(blockSize), ts)
		return mockBlock, nil
	}
	md := testNamespaceMetadata(blockSize, 4*time.Hour)
	idx, err := newNamespaceIndexWithNewBlockFn(md, newBlockFn, opts)
	require.NoError(t, err)

	defer func() {
		require.NoError(t, idx.Close())
	}()

	id := ident.StringID("foo")
	tag := ident.StringTag("name", "value")
	tags := ident.NewTags(tag)
	lifecycle := index.NewMockOnIndexSeries(ctrl)
	mockBlock.EXPECT().
		WriteBatch(gomock.Any()).
		Return(index.WriteBatchResult{}, nil).
		Do(func(batch *index.WriteBatch) {
			docs := batch.PendingDocs()
			require.Equal(t, 1, len(docs))
			require.Equal(t, doc.Document{
				ID:     id.Bytes(),
				Fields: doc.Fields{{Name: tag.Name.Bytes(), Value: tag.Value.Bytes()}},
			}, docs[0])
			entries := batch.PendingEntries()
			require.Equal(t, 1, len(entries))
			require.True(t, entries[0].Timestamp.Equal(now))
			require.True(t, entries[0].OnIndexSeries == lifecycle) // Just ptr equality
		})
	batch := index.NewWriteBatch(index.WriteBatchOptions{
		IndexBlockSize: blockSize,
	})
	batch.Append(testWriteBatchEntry(id, tags, now, lifecycle))
	require.NoError(t, idx.WriteBatch(batch))
}

func TestNamespaceIndexWriteCreatesBlock(t *testing.T) {
	ctrl := gomock.NewController(xtest.Reporter{T: t})
	defer ctrl.Finish()

	blockSize := time.Hour
	now := time.Now().Truncate(blockSize).Add(2 * time.Minute)
	t0 := now.Truncate(blockSize)
	t1 := t0.Add(blockSize)
	var nowLock sync.Mutex
	nowFn := func() time.Time {
		nowLock.Lock()
		defer nowLock.Unlock()
		return now
	}
	opts := DefaultTestOptions()
	opts = opts.SetClockOptions(opts.ClockOptions().SetNowFn(nowFn))

	b0 := index.NewMockBlock(ctrl)
	b0.EXPECT().Stats(gomock.Any()).Return(nil).AnyTimes()
	b0.EXPECT().Close().Return(nil)
	b0.EXPECT().StartTime().Return(t0).AnyTimes()
	b1 := index.NewMockBlock(ctrl)
	b1.EXPECT().Stats(gomock.Any()).Return(nil).AnyTimes()
	b1.EXPECT().Close().Return(nil)
	b1.EXPECT().StartTime().Return(t1).AnyTimes()
	newBlockFn := func(
		ts time.Time,
		md namespace.Metadata,
		_ index.BlockOptions,
		io index.Options,
	) (index.Block, error) {
		if ts.Equal(t0) {
			return b0, nil
		}
		if ts.Equal(t1) {
			return b1, nil
		}
		panic("should never get here")
	}
	md := testNamespaceMetadata(blockSize, 4*time.Hour)
	idx, err := newNamespaceIndexWithNewBlockFn(md, newBlockFn, opts)
	require.NoError(t, err)

	defer func() {
		require.NoError(t, idx.Close())
	}()

	id := ident.StringID("foo")
	tag := ident.StringTag("name", "value")
	tags := ident.NewTags(tag)
	lifecycle := index.NewMockOnIndexSeries(ctrl)
	b1.EXPECT().
		WriteBatch(gomock.Any()).
		Return(index.WriteBatchResult{}, nil).
		Do(func(batch *index.WriteBatch) {
			docs := batch.PendingDocs()
			require.Equal(t, 1, len(docs))
			require.Equal(t, doc.Document{
				ID:     id.Bytes(),
				Fields: doc.Fields{{Name: tag.Name.Bytes(), Value: tag.Value.Bytes()}},
			}, docs[0])
			entries := batch.PendingEntries()
			require.Equal(t, 1, len(entries))
			require.True(t, entries[0].Timestamp.Equal(now))
			require.True(t, entries[0].OnIndexSeries == lifecycle) // Just ptr equality
		})

	nowLock.Lock()
	now = now.Add(blockSize)
	nowLock.Unlock()

	entry, doc := testWriteBatchEntry(id, tags, now, lifecycle)
	batch := testWriteBatch(entry, doc, testWriteBatchBlockSizeOption(blockSize))
	require.NoError(t, idx.WriteBatch(batch))
}

func TestNamespaceIndexBootstrap(t *testing.T) {
	ctrl := gomock.NewController(xtest.Reporter{T: t})
	defer ctrl.Finish()

	blockSize := time.Hour
	now := time.Now().Truncate(blockSize).Add(2 * time.Minute)
	t0 := now.Truncate(blockSize)
	t0Nanos := xtime.ToUnixNano(t0)
	t1 := t0.Add(1 * blockSize)
	t1Nanos := xtime.ToUnixNano(t1)
	t2 := t1.Add(1 * blockSize)
	var nowLock sync.Mutex
	nowFn := func() time.Time {
		nowLock.Lock()
		defer nowLock.Unlock()
		return now
	}
	opts := DefaultTestOptions()
	opts = opts.SetClockOptions(opts.ClockOptions().SetNowFn(nowFn))

	b0 := index.NewMockBlock(ctrl)
	b0.EXPECT().Stats(gomock.Any()).Return(nil).AnyTimes()
	b0.EXPECT().StartTime().Return(t0).AnyTimes()
	b1 := index.NewMockBlock(ctrl)
	b1.EXPECT().Stats(gomock.Any()).Return(nil).AnyTimes()
	b1.EXPECT().StartTime().Return(t1).AnyTimes()
	newBlockFn := func(
		ts time.Time,
		md namespace.Metadata,
		_ index.BlockOptions,
		io index.Options,
	) (index.Block, error) {
		if ts.Equal(t0) {
			return b0, nil
		}
		if ts.Equal(t1) {
			return b1, nil
		}
		panic("should never get here")
	}
	md := testNamespaceMetadata(blockSize, 4*time.Hour)
	idx, err := newNamespaceIndexWithNewBlockFn(md, newBlockFn, opts)
	require.NoError(t, err)

	seg1 := segment.NewMockSegment(ctrl)
	seg2 := segment.NewMockSegment(ctrl)
	seg3 := segment.NewMockSegment(ctrl)
	bootstrapResults := result.IndexResults{
		t0Nanos: result.NewIndexBlock(t0, []segment.Segment{seg1}, result.NewShardTimeRanges(t0, t1, 1, 2, 3)),
		t1Nanos: result.NewIndexBlock(t1, []segment.Segment{seg2, seg3}, result.NewShardTimeRanges(t1, t2, 1, 2, 3)),
	}

	b0.EXPECT().AddResults(bootstrapResults[t0Nanos]).Return(nil)
	b1.EXPECT().AddResults(bootstrapResults[t1Nanos]).Return(nil)
	require.NoError(t, idx.Bootstrap(bootstrapResults))
}

func TestNamespaceIndexTickExpire(t *testing.T) {
	ctrl := gomock.NewController(xtest.Reporter{T: t})
	defer ctrl.Finish()

	retentionPeriod := 4 * time.Hour
	blockSize := time.Hour
	now := time.Now().Truncate(blockSize).Add(2 * time.Minute)
	t0 := now.Truncate(blockSize)
	var nowLock sync.Mutex
	nowFn := func() time.Time {
		nowLock.Lock()
		defer nowLock.Unlock()
		return now
	}
	opts := DefaultTestOptions()
	opts = opts.SetClockOptions(opts.ClockOptions().SetNowFn(nowFn))

	b0 := index.NewMockBlock(ctrl)
	b0.EXPECT().Stats(gomock.Any()).Return(nil).AnyTimes()
	b0.EXPECT().StartTime().Return(t0).AnyTimes()
	newBlockFn := func(
		ts time.Time,
		md namespace.Metadata,
		_ index.BlockOptions,
		io index.Options,
	) (index.Block, error) {
		if ts.Equal(t0) {
			return b0, nil
		}
		panic("should never get here")
	}
	md := testNamespaceMetadata(blockSize, retentionPeriod)
	idx, err := newNamespaceIndexWithNewBlockFn(md, newBlockFn, opts)
	require.NoError(t, err)

	nowLock.Lock()
	now = now.Add(retentionPeriod).Add(blockSize)
	nowLock.Unlock()

	c := context.NewCancellable()
	b0.EXPECT().Close().Return(nil)
	result, err := idx.Tick(c, nowFn())
	require.NoError(t, err)
	require.Equal(t, namespaceIndexTickResult{
		NumBlocksEvicted: 1,
	}, result)
}

func TestNamespaceIndexTick(t *testing.T) {
	ctrl := gomock.NewController(xtest.Reporter{T: t})
	defer ctrl.Finish()

	retentionPeriod := 4 * time.Hour
	blockSize := time.Hour
	now := time.Now().Truncate(blockSize).Add(2 * time.Minute)
	t0 := now.Truncate(blockSize)
	var nowLock sync.Mutex
	nowFn := func() time.Time {
		nowLock.Lock()
		defer nowLock.Unlock()
		return now
	}
	opts := DefaultTestOptions()
	opts = opts.SetClockOptions(opts.ClockOptions().SetNowFn(nowFn))

	b0 := index.NewMockBlock(ctrl)
	b0.EXPECT().Stats(gomock.Any()).Return(nil).AnyTimes()
	b0.EXPECT().Close().Return(nil)
	b0.EXPECT().StartTime().Return(t0).AnyTimes()
	newBlockFn := func(
		ts time.Time,
		md namespace.Metadata,
		_ index.BlockOptions,
		io index.Options,
	) (index.Block, error) {
		if ts.Equal(t0) {
			return b0, nil
		}
		panic("should never get here")
	}
	md := testNamespaceMetadata(blockSize, retentionPeriod)
	idx, err := newNamespaceIndexWithNewBlockFn(md, newBlockFn, opts)
	require.NoError(t, err)

	defer func() {
		require.NoError(t, idx.Close())
	}()

	c := context.NewCancellable()
	b0.EXPECT().Tick(c, nowFn()).Return(index.BlockTickResult{
		NumDocs:     10,
		NumSegments: 2,
	}, nil)
	result, err := idx.Tick(c, nowFn())
	require.NoError(t, err)
	require.Equal(t, namespaceIndexTickResult{
		NumBlocks:    1,
		NumSegments:  2,
		NumTotalDocs: 10,
	}, result)

	nowLock.Lock()
	now = now.Add(2 * blockSize)
	nowLock.Unlock()

	b0.EXPECT().Tick(c, nowFn()).Return(index.BlockTickResult{
		NumDocs:     10,
		NumSegments: 2,
	}, nil)
	b0.EXPECT().IsSealed().Return(false)
	b0.EXPECT().Seal().Return(nil)
	result, err = idx.Tick(c, nowFn())
	require.NoError(t, err)
	require.Equal(t, namespaceIndexTickResult{
		NumBlocks:       1,
		NumBlocksSealed: 1,
		NumSegments:     2,
		NumTotalDocs:    10,
	}, result)

	b0.EXPECT().Tick(c, nowFn()).Return(index.BlockTickResult{
		NumDocs:     10,
		NumSegments: 2,
	}, nil)
	b0.EXPECT().IsSealed().Return(true)
	result, err = idx.Tick(c, nowFn())
	require.NoError(t, err)
	require.Equal(t, namespaceIndexTickResult{
		NumBlocks:    1,
		NumSegments:  2,
		NumTotalDocs: 10,
	}, result)
}

func TestNamespaceIndexBlockQuery(t *testing.T) {
	ctrl := gomock.NewController(xtest.Reporter{T: t})
	defer ctrl.Finish()

	retention := 2 * time.Hour
	blockSize := time.Hour
	now := time.Now().Truncate(blockSize).Add(10 * time.Minute)
	t0 := now.Truncate(blockSize)
	t0Nanos := xtime.ToUnixNano(t0)
	t1 := t0.Add(1 * blockSize)
	t1Nanos := xtime.ToUnixNano(t1)
	t2 := t1.Add(1 * blockSize)
	var nowLock sync.Mutex
	nowFn := func() time.Time {
		nowLock.Lock()
		defer nowLock.Unlock()
		return now
	}
	opts := DefaultTestOptions()
	opts = opts.SetClockOptions(opts.ClockOptions().SetNowFn(nowFn))

	b0 := index.NewMockBlock(ctrl)
	b0.EXPECT().Stats(gomock.Any()).Return(nil).AnyTimes()
	b0.EXPECT().Close().Return(nil)
	b0.EXPECT().StartTime().Return(t0).AnyTimes()
	b0.EXPECT().EndTime().Return(t0.Add(blockSize)).AnyTimes()
	b1 := index.NewMockBlock(ctrl)
	b1.EXPECT().Stats(gomock.Any()).Return(nil).AnyTimes()
	b1.EXPECT().Close().Return(nil)
	b1.EXPECT().StartTime().Return(t1).AnyTimes()
	b1.EXPECT().EndTime().Return(t1.Add(blockSize)).AnyTimes()
	newBlockFn := func(
		ts time.Time,
		md namespace.Metadata,
		_ index.BlockOptions,
		io index.Options,
	) (index.Block, error) {
		if ts.Equal(t0) {
			return b0, nil
		}
		if ts.Equal(t1) {
			return b1, nil
		}
		panic("should never get here")
	}
	md := testNamespaceMetadata(blockSize, retention)
	idx, err := newNamespaceIndexWithNewBlockFn(md, newBlockFn, opts)
	require.NoError(t, err)

	defer func() {
		require.NoError(t, idx.Close())
	}()

	seg1 := segment.NewMockSegment(ctrl)
	seg2 := segment.NewMockSegment(ctrl)
	seg3 := segment.NewMockSegment(ctrl)
	bootstrapResults := result.IndexResults{
		t0Nanos: result.NewIndexBlock(t0, []segment.Segment{seg1}, result.NewShardTimeRanges(t0, t1, 1, 2, 3)),
		t1Nanos: result.NewIndexBlock(t1, []segment.Segment{seg2, seg3}, result.NewShardTimeRanges(t1, t2, 1, 2, 3)),
	}

	b0.EXPECT().AddResults(bootstrapResults[t0Nanos]).Return(nil)
	b1.EXPECT().AddResults(bootstrapResults[t1Nanos]).Return(nil)
	require.NoError(t, idx.Bootstrap(bootstrapResults))

	// only queries as much as is needed (wrt to time)
	ctx := context.NewContext()
	q := defaultQuery
	qOpts := index.QueryOptions{
		StartInclusive: t0,
		EndExclusive:   now.Add(time.Minute),
	}

	// create initial span from a mock tracer and get ctx
	mtr := mocktracer.New()
	sp := mtr.StartSpan("root")
	ctx.SetGoContext(opentracing.ContextWithSpan(stdlibctx.Background(), sp))

	b0.EXPECT().Query(gomock.Any(), gomock.Any(), q, qOpts, gomock.Any(), gomock.Any()).Return(true, nil)
	_, err = idx.Query(ctx, q, qOpts)
	require.NoError(t, err)

	// queries multiple blocks if needed
	qOpts = index.QueryOptions{
		StartInclusive: t0,
		EndExclusive:   t2.Add(time.Minute),
	}
	b0.EXPECT().Query(gomock.Any(), gomock.Any(), q, qOpts, gomock.Any(), gomock.Any()).Return(true, nil)
	b1.EXPECT().Query(gomock.Any(), gomock.Any(), q, qOpts, gomock.Any(), gomock.Any()).Return(true, nil)
	_, err = idx.Query(ctx, q, qOpts)
	require.NoError(t, err)

	// stops querying once a block returns non-exhaustive
	qOpts = index.QueryOptions{
		StartInclusive: t0,
		EndExclusive:   t0.Add(time.Minute),
	}
	b0.EXPECT().Query(gomock.Any(), gomock.Any(), q, qOpts, gomock.Any(), gomock.Any()).Return(false, nil)
	_, err = idx.Query(ctx, q, qOpts)
	require.NoError(t, err)

	sp.Finish()
	spans := mtr.FinishedSpans()
	require.Len(t, spans, 11)
}

func TestNamespaceIndexBlockQueryReleasingContext(t *testing.T) {
	ctrl := gomock.NewController(xtest.Reporter{T: t})
	defer ctrl.Finish()

	retention := 2 * time.Hour
	blockSize := time.Hour
	now := time.Now().Truncate(blockSize).Add(10 * time.Minute)
	t0 := now.Truncate(blockSize)
	t0Nanos := xtime.ToUnixNano(t0)
	t1 := t0.Add(1 * blockSize)
	t1Nanos := xtime.ToUnixNano(t1)
	t2 := t1.Add(1 * blockSize)
	var nowLock sync.Mutex
	nowFn := func() time.Time {
		nowLock.Lock()
		defer nowLock.Unlock()
		return now
	}
	opts := DefaultTestOptions()
	opts = opts.SetClockOptions(opts.ClockOptions().SetNowFn(nowFn))

	b0 := index.NewMockBlock(ctrl)
	b0.EXPECT().Stats(gomock.Any()).Return(nil).AnyTimes()
	b0.EXPECT().Close().Return(nil)
	b0.EXPECT().StartTime().Return(t0).AnyTimes()
	b0.EXPECT().EndTime().Return(t0.Add(blockSize)).AnyTimes()
	b1 := index.NewMockBlock(ctrl)
	b1.EXPECT().Stats(gomock.Any()).Return(nil).AnyTimes()
	b1.EXPECT().Close().Return(nil)
	b1.EXPECT().StartTime().Return(t1).AnyTimes()
	b1.EXPECT().EndTime().Return(t1.Add(blockSize)).AnyTimes()
	newBlockFn := func(
		ts time.Time,
		md namespace.Metadata,
		_ index.BlockOptions,
		io index.Options,
	) (index.Block, error) {
		if ts.Equal(t0) {
			return b0, nil
		}
		if ts.Equal(t1) {
			return b1, nil
		}
		panic("should never get here")
	}

	iopts := opts.IndexOptions()
	mockPool := index.NewMockQueryResultsPool(ctrl)
	iopts = iopts.SetQueryResultsPool(mockPool)
	stubResult := index.NewQueryResults(ident.StringID("ns"), index.QueryResultsOptions{}, iopts)

	md := testNamespaceMetadata(blockSize, retention)
	idxIface, err := newNamespaceIndexWithNewBlockFn(md, newBlockFn, opts)
	require.NoError(t, err)

	idx, ok := idxIface.(*nsIndex)
	require.True(t, ok)
	idx.resultsPool = mockPool

	defer func() {
		require.NoError(t, idx.Close())
	}()

	seg1 := segment.NewMockSegment(ctrl)
	seg2 := segment.NewMockSegment(ctrl)
	seg3 := segment.NewMockSegment(ctrl)
	bootstrapResults := result.IndexResults{
		t0Nanos: result.NewIndexBlock(t0, []segment.Segment{seg1}, result.NewShardTimeRanges(t0, t1, 1, 2, 3)),
		t1Nanos: result.NewIndexBlock(t1, []segment.Segment{seg2, seg3}, result.NewShardTimeRanges(t1, t2, 1, 2, 3)),
	}

	b0.EXPECT().AddResults(bootstrapResults[t0Nanos]).Return(nil)
	b1.EXPECT().AddResults(bootstrapResults[t1Nanos]).Return(nil)
	require.NoError(t, idx.Bootstrap(bootstrapResults))

	ctx := context.NewContext()
	q := defaultQuery
	qOpts := index.QueryOptions{
		StartInclusive: t0,
		EndExclusive:   now.Add(time.Minute),
	}
	gomock.InOrder(
		mockPool.EXPECT().Get().Return(stubResult),
		b0.EXPECT().Query(ctx, gomock.Any(), q, qOpts, gomock.Any(), gomock.Any()).Return(true, nil),
		mockPool.EXPECT().Put(stubResult),
	)
	_, err = idx.Query(ctx, q, qOpts)
	require.NoError(t, err)
	ctx.BlockingClose()
}

func TestNamespaceIndexBlockAggregateQuery(t *testing.T) {
	ctrl := gomock.NewController(xtest.Reporter{T: t})
	defer ctrl.Finish()

	query := idx.NewTermQuery([]byte("a"), []byte("b"))
	retention := 2 * time.Hour
	blockSize := time.Hour
	now := time.Now().Truncate(blockSize).Add(10 * time.Minute)
	t0 := now.Truncate(blockSize)
	t0Nanos := xtime.ToUnixNano(t0)
	t1 := t0.Add(1 * blockSize)
	t1Nanos := xtime.ToUnixNano(t1)
	t2 := t1.Add(1 * blockSize)
	var nowLock sync.Mutex
	nowFn := func() time.Time {
		nowLock.Lock()
		defer nowLock.Unlock()
		return now
	}
	opts := DefaultTestOptions()
	opts = opts.SetClockOptions(opts.ClockOptions().SetNowFn(nowFn))

	b0 := index.NewMockBlock(ctrl)
	b0.EXPECT().Stats(gomock.Any()).Return(nil).AnyTimes()
	b0.EXPECT().Close().Return(nil)
	b0.EXPECT().StartTime().Return(t0).AnyTimes()
	b0.EXPECT().EndTime().Return(t0.Add(blockSize)).AnyTimes()
	b1 := index.NewMockBlock(ctrl)
	b1.EXPECT().Stats(gomock.Any()).Return(nil).AnyTimes()
	b1.EXPECT().Close().Return(nil)
	b1.EXPECT().StartTime().Return(t1).AnyTimes()
	b1.EXPECT().EndTime().Return(t1.Add(blockSize)).AnyTimes()
	newBlockFn := func(
		ts time.Time,
		md namespace.Metadata,
		_ index.BlockOptions,
		io index.Options,
	) (index.Block, error) {
		if ts.Equal(t0) {
			return b0, nil
		}
		if ts.Equal(t1) {
			return b1, nil
		}
		panic("should never get here")
	}
	md := testNamespaceMetadata(blockSize, retention)
	idx, err := newNamespaceIndexWithNewBlockFn(md, newBlockFn, opts)
	require.NoError(t, err)

	defer func() {
		require.NoError(t, idx.Close())
	}()

	seg1 := segment.NewMockSegment(ctrl)
	seg2 := segment.NewMockSegment(ctrl)
	seg3 := segment.NewMockSegment(ctrl)
	bootstrapResults := result.IndexResults{
		t0Nanos: result.NewIndexBlock(t0, []segment.Segment{seg1}, result.NewShardTimeRanges(t0, t1, 1, 2, 3)),
		t1Nanos: result.NewIndexBlock(t1, []segment.Segment{seg2, seg3}, result.NewShardTimeRanges(t1, t2, 1, 2, 3)),
	}

	b0.EXPECT().AddResults(bootstrapResults[t0Nanos]).Return(nil)
	b1.EXPECT().AddResults(bootstrapResults[t1Nanos]).Return(nil)
	require.NoError(t, idx.Bootstrap(bootstrapResults))

	// only queries as much as is needed (wrt to time)
	ctx := context.NewContext()

	// create initial span from a mock tracer and get ctx
	mtr := mocktracer.New()
	sp := mtr.StartSpan("root")
	ctx.SetGoContext(opentracing.ContextWithSpan(stdlibctx.Background(), sp))

	q := index.Query{
		Query: query,
	}
	qOpts := index.QueryOptions{
		StartInclusive: t0,
		EndExclusive:   now.Add(time.Minute),
	}
	aggOpts := index.AggregationOptions{QueryOptions: qOpts}

	b0.EXPECT().Query(gomock.Any(), gomock.Any(), q, qOpts, gomock.Any(), gomock.Any()).Return(true, nil)
	_, err = idx.AggregateQuery(ctx, q, aggOpts)
	require.NoError(t, err)

	// queries multiple blocks if needed
	qOpts = index.QueryOptions{
		StartInclusive: t0,
		EndExclusive:   t2.Add(time.Minute),
	}
	aggOpts = index.AggregationOptions{QueryOptions: qOpts}
	b0.EXPECT().Query(gomock.Any(), gomock.Any(), q, qOpts, gomock.Any(), gomock.Any()).Return(true, nil)
	b1.EXPECT().Query(gomock.Any(), gomock.Any(), q, qOpts, gomock.Any(), gomock.Any()).Return(true, nil)
	_, err = idx.AggregateQuery(ctx, q, aggOpts)
	require.NoError(t, err)

	// stops querying once a block returns non-exhaustive
	qOpts = index.QueryOptions{
		StartInclusive: t0,
		EndExclusive:   t0.Add(time.Minute),
	}
	b0.EXPECT().Query(gomock.Any(), gomock.Any(), q, qOpts, gomock.Any(), gomock.Any()).Return(false, nil)
	aggOpts = index.AggregationOptions{QueryOptions: qOpts}
	_, err = idx.AggregateQuery(ctx, q, aggOpts)
	require.NoError(t, err)

	sp.Finish()
	spans := mtr.FinishedSpans()
	require.Len(t, spans, 11)
}

func TestNamespaceIndexBlockAggregateQueryReleasingContext(t *testing.T) {
	ctrl := gomock.NewController(xtest.Reporter{T: t})
	defer ctrl.Finish()

	retention := 2 * time.Hour
	blockSize := time.Hour
	now := time.Now().Truncate(blockSize).Add(10 * time.Minute)
	t0 := now.Truncate(blockSize)
	t0Nanos := xtime.ToUnixNano(t0)
	t1 := t0.Add(1 * blockSize)
	t1Nanos := xtime.ToUnixNano(t1)
	t2 := t1.Add(1 * blockSize)
	var nowLock sync.Mutex
	nowFn := func() time.Time {
		nowLock.Lock()
		defer nowLock.Unlock()
		return now
	}
	opts := DefaultTestOptions()
	opts = opts.SetClockOptions(opts.ClockOptions().SetNowFn(nowFn))

	query := idx.NewTermQuery([]byte("a"), []byte("b"))
	b0 := index.NewMockBlock(ctrl)
	b0.EXPECT().Stats(gomock.Any()).Return(nil).AnyTimes()
	b0.EXPECT().Close().Return(nil)
	b0.EXPECT().StartTime().Return(t0).AnyTimes()
	b0.EXPECT().EndTime().Return(t0.Add(blockSize)).AnyTimes()
	b1 := index.NewMockBlock(ctrl)
	b1.EXPECT().Stats(gomock.Any()).Return(nil).AnyTimes()
	b1.EXPECT().Close().Return(nil)
	b1.EXPECT().StartTime().Return(t1).AnyTimes()
	b1.EXPECT().EndTime().Return(t1.Add(blockSize)).AnyTimes()
	newBlockFn := func(
		ts time.Time,
		md namespace.Metadata,
		_ index.BlockOptions,
		io index.Options,
	) (index.Block, error) {
		if ts.Equal(t0) {
			return b0, nil
		}
		if ts.Equal(t1) {
			return b1, nil
		}
		panic("should never get here")
	}

	iopts := opts.IndexOptions()
	mockPool := index.NewMockAggregateResultsPool(ctrl)
	iopts = iopts.SetAggregateResultsPool(mockPool)
	stubResult := index.NewAggregateResults(ident.StringID("ns"), index.AggregateResultsOptions{}, iopts)

	md := testNamespaceMetadata(blockSize, retention)
	idxIface, err := newNamespaceIndexWithNewBlockFn(md, newBlockFn, opts)
	require.NoError(t, err)

	idx, ok := idxIface.(*nsIndex)
	require.True(t, ok)
	idx.aggregateResultsPool = mockPool

	defer func() {
		require.NoError(t, idx.Close())
	}()

	seg1 := segment.NewMockSegment(ctrl)
	seg2 := segment.NewMockSegment(ctrl)
	seg3 := segment.NewMockSegment(ctrl)
	bootstrapResults := result.IndexResults{
		t0Nanos: result.NewIndexBlock(t0, []segment.Segment{seg1}, result.NewShardTimeRanges(t0, t1, 1, 2, 3)),
		t1Nanos: result.NewIndexBlock(t1, []segment.Segment{seg2, seg3}, result.NewShardTimeRanges(t1, t2, 1, 2, 3)),
	}

	b0.EXPECT().AddResults(bootstrapResults[t0Nanos]).Return(nil)
	b1.EXPECT().AddResults(bootstrapResults[t1Nanos]).Return(nil)
	require.NoError(t, idx.Bootstrap(bootstrapResults))

	// only queries as much as is needed (wrt to time)
	ctx := context.NewContext()
	q := index.Query{
		Query: query,
	}
	qOpts := index.QueryOptions{
		StartInclusive: t0,
		EndExclusive:   now.Add(time.Minute),
	}
	aggOpts := index.AggregationOptions{QueryOptions: qOpts}

	gomock.InOrder(
		mockPool.EXPECT().Get().Return(stubResult),
		b0.EXPECT().Query(ctx, gomock.Any(), q, qOpts, gomock.Any(), gomock.Any()).Return(true, nil),
		mockPool.EXPECT().Put(stubResult),
	)
	_, err = idx.AggregateQuery(ctx, q, aggOpts)
	require.NoError(t, err)
	ctx.BlockingClose()
}

func TestNamespaceIndexBlockAggregateQueryAggPath(t *testing.T) {
	ctrl := gomock.NewController(xtest.Reporter{T: t})
	defer ctrl.Finish()

	queries := []idx.Query{idx.NewAllQuery(), idx.NewFieldQuery([]byte("field"))}
	retention := 2 * time.Hour
	blockSize := time.Hour
	now := time.Now().Truncate(blockSize).Add(10 * time.Minute)
	t0 := now.Truncate(blockSize)
	t0Nanos := xtime.ToUnixNano(t0)
	t1 := t0.Add(1 * blockSize)
	t1Nanos := xtime.ToUnixNano(t1)
	t2 := t1.Add(1 * blockSize)
	var nowLock sync.Mutex
	nowFn := func() time.Time {
		nowLock.Lock()
		defer nowLock.Unlock()
		return now
	}
	opts := DefaultTestOptions()
	opts = opts.SetClockOptions(opts.ClockOptions().SetNowFn(nowFn))

	b0 := index.NewMockBlock(ctrl)
	b0.EXPECT().Stats(gomock.Any()).Return(nil).AnyTimes()
	b0.EXPECT().Close().Return(nil)
	b0.EXPECT().StartTime().Return(t0).AnyTimes()
	b0.EXPECT().EndTime().Return(t1).AnyTimes()
	b1 := index.NewMockBlock(ctrl)
	b1.EXPECT().Stats(gomock.Any()).Return(nil).AnyTimes()
	b1.EXPECT().Close().Return(nil)
	b1.EXPECT().StartTime().Return(t1).AnyTimes()
	b1.EXPECT().EndTime().Return(t2).AnyTimes()
	newBlockFn := func(
		ts time.Time,
		md namespace.Metadata,
		_ index.BlockOptions,
		io index.Options,
	) (index.Block, error) {
		if ts.Equal(t0) {
			return b0, nil
		}
		if ts.Equal(t1) {
			return b1, nil
		}
		panic("should never get here")
	}
	md := testNamespaceMetadata(blockSize, retention)
	idx, err := newNamespaceIndexWithNewBlockFn(md, newBlockFn, opts)
	require.NoError(t, err)

	defer func() {
		require.NoError(t, idx.Close())
	}()

	seg1 := segment.NewMockSegment(ctrl)
	seg2 := segment.NewMockSegment(ctrl)
	seg3 := segment.NewMockSegment(ctrl)
	bootstrapResults := result.IndexResults{
		t0Nanos: result.NewIndexBlock(t0, []segment.Segment{seg1}, result.NewShardTimeRanges(t0, t1, 1, 2, 3)),
		t1Nanos: result.NewIndexBlock(t1, []segment.Segment{seg2, seg3}, result.NewShardTimeRanges(t1, t2, 1, 2, 3)),
	}

	b0.EXPECT().AddResults(bootstrapResults[t0Nanos]).Return(nil)
	b1.EXPECT().AddResults(bootstrapResults[t1Nanos]).Return(nil)
	require.NoError(t, idx.Bootstrap(bootstrapResults))

	// only queries as much as is needed (wrt to time)
	ctx := context.NewContext()

	qOpts := index.QueryOptions{
		StartInclusive: t0,
		EndExclusive:   now.Add(time.Minute),
	}
	aggOpts := index.AggregationOptions{QueryOptions: qOpts}

	for _, query := range queries {
		q := index.Query{
			Query: query,
		}
		b0.EXPECT().Aggregate(ctx, gomock.Any(), qOpts, gomock.Any(), gomock.Any()).Return(true, nil)
		_, err = idx.AggregateQuery(ctx, q, aggOpts)
		require.NoError(t, err)

		// queries multiple blocks if needed
		qOpts = index.QueryOptions{
			StartInclusive: t0,
			EndExclusive:   t2.Add(time.Minute),
		}
		aggOpts = index.AggregationOptions{QueryOptions: qOpts}
		b0.EXPECT().Aggregate(ctx, gomock.Any(), qOpts, gomock.Any(), gomock.Any()).Return(true, nil)
		b1.EXPECT().Aggregate(ctx, gomock.Any(), qOpts, gomock.Any(), gomock.Any()).Return(true, nil)
		_, err = idx.AggregateQuery(ctx, q, aggOpts)
		require.NoError(t, err)

		// stops querying once a block returns non-exhaustive
		qOpts = index.QueryOptions{
			StartInclusive: t0,
			EndExclusive:   t0.Add(time.Minute),
		}
		b0.EXPECT().Aggregate(ctx, gomock.Any(), qOpts, gomock.Any(), gomock.Any()).Return(false, nil)
		aggOpts = index.AggregationOptions{QueryOptions: qOpts}
		_, err = idx.AggregateQuery(ctx, q, aggOpts)
		require.NoError(t, err)
	}
}
