// Copyright (c) 2016 Uber Technologies, Inc.
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
	"bytes"
	"sync"
	"time"

	"github.com/m3db/m3/src/dbnode/clock"
	"github.com/m3db/m3/src/dbnode/encoding"
	"github.com/m3db/m3/src/dbnode/namespace"
	"github.com/m3db/m3/src/dbnode/persist"
	"github.com/m3db/m3/src/dbnode/persist/fs"
	"github.com/m3db/m3/src/dbnode/persist/fs/commitlog"
	"github.com/m3db/m3/src/dbnode/runtime"
	"github.com/m3db/m3/src/dbnode/sharding"
	"github.com/m3db/m3/src/dbnode/storage/block"
	"github.com/m3db/m3/src/dbnode/storage/bootstrap"
	"github.com/m3db/m3/src/dbnode/storage/bootstrap/result"
	"github.com/m3db/m3/src/dbnode/storage/index"
	"github.com/m3db/m3/src/dbnode/storage/repair"
	"github.com/m3db/m3/src/dbnode/storage/series"
	"github.com/m3db/m3/src/dbnode/ts"
	"github.com/m3db/m3/src/dbnode/x/xio"
	"github.com/m3db/m3/src/x/context"
	"github.com/m3db/m3/src/x/ident"
	"github.com/m3db/m3/src/x/instrument"
	"github.com/m3db/m3/src/x/pool"
	xsync "github.com/m3db/m3/src/x/sync"
	xtime "github.com/m3db/m3/src/x/time"
)

// PageToken is an opaque paging token.
type PageToken []byte

// IndexedErrorHandler can handle individual errors based on their index. It
// is used primarily in cases where we need to handle errors in batches, but
// want to avoid an intermediary allocation of []error.
type IndexedErrorHandler interface {
	HandleError(index int, err error)
}

// Database is a time series database.
type Database interface {
	// Options returns the database options.
	Options() Options

	// AssignShardSet sets the shard set assignment and returns immediately.
	AssignShardSet(shardSet sharding.ShardSet)

	// Namespaces returns the namespaces.
	Namespaces() []Namespace

	// Namespace returns the specified namespace.
	Namespace(ns ident.ID) (Namespace, bool)

	// Open will open the database for writing and reading.
	Open() error

	// Close will close the database for writing and reading. Close releases
	// release resources held by owned namespaces.
	Close() error

	// ShardSet returns the set of shards currently associated with
	// this namespace.
	ShardSet() sharding.ShardSet

	// Terminate will close the database for writing and reading. Terminate does
	// NOT release any resources held by owned namespaces, instead relying upon
	// the GC to do so.
	Terminate() error

	// Write value to the database for an ID.
	Write(
		ctx context.Context,
		namespace ident.ID,
		id ident.ID,
		timestamp time.Time,
		value float64,
		unit xtime.Unit,
		annotation []byte,
	) error

	// WriteTagged values to the database for an ID.
	WriteTagged(
		ctx context.Context,
		namespace ident.ID,
		id ident.ID,
		tags ident.TagIterator,
		timestamp time.Time,
		value float64,
		unit xtime.Unit,
		annotation []byte,
	) error

	// BatchWriter returns a batch writer for the provided namespace that can
	// be used to issue a batch of writes to either WriteBatch
	// or WriteTaggedBatch.
	//
	// Note that when using the BatchWriter the caller owns the lifecycle of the series
	// IDs and tag iterators (I.E) if they're being pooled its the callers responsibility
	// to return them to the appropriate pool, but the annotations are owned by the
	// ts.WriteBatch itself and will be finalized when the entire ts.WriteBatch is finalized
	// due to their lifecycle being more complicated. Callers can still control the pooling
	// of the annotations by using the SetFinalizeAnnotationFn on the WriteBatch itself.
	BatchWriter(namespace ident.ID, batchSize int) (ts.BatchWriter, error)

	// WriteBatch is the same as Write, but in batch.
	WriteBatch(
		ctx context.Context,
		namespace ident.ID,
		writes ts.BatchWriter,
		errHandler IndexedErrorHandler,
	) error

	// WriteTaggedBatch is the same as WriteTagged, but in batch.
	WriteTaggedBatch(
		ctx context.Context,
		namespace ident.ID,
		writes ts.BatchWriter,
		errHandler IndexedErrorHandler,
	) error

	// QueryIDs resolves the given query into known IDs.
	QueryIDs(
		ctx context.Context,
		namespace ident.ID,
		query index.Query,
		opts index.QueryOptions,
	) (index.QueryResult, error)

	// AggregateQuery resolves the given query into aggregated tags.
	AggregateQuery(
		ctx context.Context,
		namespace ident.ID,
		query index.Query,
		opts index.AggregationOptions,
	) (index.AggregateQueryResult, error)

	// ReadEncoded retrieves encoded segments for an ID
	ReadEncoded(
		ctx context.Context,
		namespace ident.ID,
		id ident.ID,
		start, end time.Time,
	) ([][]xio.BlockReader, error)

	// FetchBlocks retrieves data blocks for a given id and a list of block
	// start times.
	FetchBlocks(
		ctx context.Context,
		namespace ident.ID,
		shard uint32,
		id ident.ID,
		starts []time.Time,
	) ([]block.FetchBlockResult, error)

	// FetchBlocksMetadata retrieves blocks metadata for a given shard, returns the
	// fetched block metadata results, the next page token, and any error encountered.
	// If we have fetched all the block metadata, we return nil as the next page token.
	FetchBlocksMetadataV2(
		ctx context.Context,
		namespace ident.ID,
		shard uint32,
		start, end time.Time,
		limit int64,
		pageToken PageToken,
		opts block.FetchBlocksMetadataOptions,
	) (block.FetchBlocksMetadataResults, PageToken, error)

	// Bootstrap bootstraps the database.
	Bootstrap() error

	// IsBootstrapped determines whether the database is bootstrapped.
	IsBootstrapped() bool

	// IsBootstrappedAndDurable determines whether the database is bootstrapped
	// and durable, meaning that it could recover all data in memory using only
	// the local disk.
	IsBootstrappedAndDurable() bool

	// IsOverloaded determines whether the database is overloaded.
	IsOverloaded() bool

	// Repair will issue a repair and return nil on success or error on error.
	Repair() error

	// Truncate truncates data for the given namespace.
	Truncate(namespace ident.ID) (int64, error)

	// BootstrapState captures and returns a snapshot of the databases'
	// bootstrap state.
	BootstrapState() DatabaseBootstrapState

	// FlushState returns the flush state for the specified shard and block start.
	FlushState(namespace ident.ID, shardID uint32, blockStart time.Time) (fileOpState, error)
}

// database is the internal database interface
type database interface {
	Database

	// GetOwnedNamespaces returns the namespaces this database owns.
	GetOwnedNamespaces() ([]databaseNamespace, error)

	// UpdateOwnedNamespaces updates the namespaces this database owns.
	UpdateOwnedNamespaces(namespaces namespace.Map) error
}

// Namespace is a time series database namespace
type Namespace interface {
	// Options returns the namespace options
	Options() namespace.Options

	// ID returns the ID of the namespace
	ID() ident.ID

	// Schema returns the schema of the namespace.
	Schema() namespace.SchemaDescr

	// NumSeries returns the number of series in the namespace
	NumSeries() int64

	// Shards returns the shard description
	Shards() []Shard
}

// NamespacesByID is a sortable slice of namespaces by ID
type NamespacesByID []Namespace

func (n NamespacesByID) Len() int      { return len(n) }
func (n NamespacesByID) Swap(i, j int) { n[i], n[j] = n[j], n[i] }
func (n NamespacesByID) Less(i, j int) bool {
	return bytes.Compare(n[i].ID().Bytes(), n[j].ID().Bytes()) < 0
}

type databaseNamespace interface {
	Namespace

	// Close will release the namespace resources and close the namespace.
	Close() error

	// AssignShardSet sets the shard set assignment and returns immediately.
	AssignShardSet(shardSet sharding.ShardSet)

	// GetOwnedShards returns the database shards.
	GetOwnedShards() []databaseShard

	// GetIndex returns the reverse index backing the namespace, if it exists.
	GetIndex() (namespaceIndex, error)

	// Tick performs any regular maintenance operations.
	Tick(c context.Cancellable, tickStart time.Time) error

	// Write writes a data point.
	Write(
		ctx context.Context,
		id ident.ID,
		timestamp time.Time,
		value float64,
		unit xtime.Unit,
		annotation []byte,
	) (ts.Series, bool, error)

	// WriteTagged values to the namespace for an ID.
	WriteTagged(
		ctx context.Context,
		id ident.ID,
		tags ident.TagIterator,
		timestamp time.Time,
		value float64,
		unit xtime.Unit,
		annotation []byte,
	) (ts.Series, bool, error)

	// QueryIDs resolves the given query into known IDs.
	QueryIDs(
		ctx context.Context,
		query index.Query,
		opts index.QueryOptions,
	) (index.QueryResult, error)

	// AggregateQuery resolves the given query into aggregated tags.
	AggregateQuery(
		ctx context.Context,
		query index.Query,
		opts index.AggregationOptions,
	) (index.AggregateQueryResult, error)

	// ReadEncoded reads data for given id within [start, end).
	ReadEncoded(
		ctx context.Context,
		id ident.ID,
		start, end time.Time,
	) ([][]xio.BlockReader, error)

	// FetchBlocks retrieves data blocks for a given id and a list of block
	// start times.
	FetchBlocks(
		ctx context.Context,
		shardID uint32,
		id ident.ID,
		starts []time.Time,
	) ([]block.FetchBlockResult, error)

	// FetchBlocksMetadata retrieves blocks metadata.
	FetchBlocksMetadataV2(
		ctx context.Context,
		shardID uint32,
		start, end time.Time,
		limit int64,
		pageToken PageToken,
		opts block.FetchBlocksMetadataOptions,
	) (block.FetchBlocksMetadataResults, PageToken, error)

	// Bootstrap performs bootstrapping.
	Bootstrap(start time.Time, process bootstrap.Process) error

	// WarmFlush flushes in-memory WarmWrites.
	WarmFlush(
		blockStart time.Time,
		ShardBootstrapStates ShardBootstrapStates,
		flush persist.FlushPreparer,
	) error

	// FlushIndex flushes in-memory index data.
	FlushIndex(
		flush persist.IndexFlush,
	) error

	// ColdFlush flushes unflushed in-memory ColdWrites.
	ColdFlush(
		flush persist.FlushPreparer,
	) error

	// Snapshot snapshots unflushed in-memory WarmWrites.
	Snapshot(blockStart, snapshotTime time.Time, flush persist.SnapshotPreparer) error

	// NeedsFlush returns true if the namespace needs a flush for the
	// period: [start, end] (both inclusive).
	// NB: The start/end times are assumed to be aligned to block size boundary.
	NeedsFlush(alignedInclusiveStart time.Time, alignedInclusiveEnd time.Time) bool

	// Truncate truncates the in-memory data for this namespace.
	Truncate() (int64, error)

	// Repair repairs the namespace data for a given time range
	Repair(repairer databaseShardRepairer, tr xtime.Range) error

	// BootstrapState captures and returns a snapshot of the namespaces'
	// bootstrap state.
	BootstrapState() ShardBootstrapStates

	// FlushState returns the flush state for the specified shard and block start.
	FlushState(shardID uint32, blockStart time.Time) (fileOpState, error)
}

// Shard is a time series database shard.
type Shard interface {
	// ID returns the ID of the shard.
	ID() uint32

	// NumSeries returns the number of series in the shard.
	NumSeries() int64

	// IsBootstrapped returns whether the shard is already bootstrapped.
	IsBootstrapped() bool

	// BootstrapState returns the shards' bootstrap state.
	BootstrapState() BootstrapState
}

type databaseShard interface {
	Shard

	// OnEvictedFromWiredList is the same as block.Owner. Had to duplicate
	// it here because mockgen chokes on embedded interfaces sometimes:
	// https://github.com/golang/mock/issues/10
	OnEvictedFromWiredList(id ident.ID, blockStart time.Time)

	// Close will release the shard resources and close the shard.
	Close() error

	// Tick performs all async updates
	Tick(c context.Cancellable, tickStart time.Time, nsCtx namespace.Context) (tickResult, error)

	Write(
		ctx context.Context,
		id ident.ID,
		timestamp time.Time,
		value float64,
		unit xtime.Unit,
		annotation []byte,
		wOpts series.WriteOptions,
	) (ts.Series, bool, error)

	// WriteTagged values to the shard for an ID.
	WriteTagged(
		ctx context.Context,
		id ident.ID,
		tags ident.TagIterator,
		timestamp time.Time,
		value float64,
		unit xtime.Unit,
		annotation []byte,
		wOpts series.WriteOptions,
	) (ts.Series, bool, error)

	ReadEncoded(
		ctx context.Context,
		id ident.ID,
		start, end time.Time,
		nsCtx namespace.Context,
	) ([][]xio.BlockReader, error)

	// FetchBlocks retrieves data blocks for a given id and a list of block
	// start times.
	FetchBlocks(
		ctx context.Context,
		id ident.ID,
		starts []time.Time,
		nsCtx namespace.Context,
	) ([]block.FetchBlockResult, error)

	// FetchBlocksForColdFlush fetches blocks for a cold flush. This function
	// informs the series and the buffer that a cold flush for the specified
	// block start is occurring so that it knows to update bucket versions.
	FetchBlocksForColdFlush(
		ctx context.Context,
		seriesID ident.ID,
		start time.Time,
		version int,
		nsCtx namespace.Context,
	) ([]xio.BlockReader, error)

	// FetchBlocksMetadataV2 retrieves blocks metadata.
	FetchBlocksMetadataV2(
		ctx context.Context,
		start, end time.Time,
		limit int64,
		pageToken PageToken,
		opts block.FetchBlocksMetadataOptions,
	) (block.FetchBlocksMetadataResults, PageToken, error)

	// Bootstrap bootstraps the shard with provided data.
	Bootstrap(
		bootstrappedSeries *result.Map,
	) error

	// WarmFlush flushes the WarmWrites in this shard.
	WarmFlush(
		blockStart time.Time,
		flush persist.FlushPreparer,
		nsCtx namespace.Context,
	) error

	// ColdFlush flushes the unflushed ColdWrites in this shard.
	ColdFlush(
		flush persist.FlushPreparer,
		resources coldFlushReuseableResources,
		nsCtx namespace.Context,
	) error

	// Snapshot snapshot's the unflushed WarmWrites in this shard.
	Snapshot(
		blockStart time.Time,
		snapshotStart time.Time,
		flush persist.SnapshotPreparer,
		nsCtx namespace.Context,
	) error

	// FlushState returns the flush state for this shard at block start.
	FlushState(blockStart time.Time) fileOpState

	// CleanupExpiredFileSets removes expired fileset files.
	CleanupExpiredFileSets(earliestToRetain time.Time) error

	// CleanupCompactedFileSets removes fileset files that have been compacted,
	// meaning that there exists a more recent, superset, fully persisted
	// fileset for that block.
	CleanupCompactedFileSets() error

	// Repair repairs the shard data for a given time.
	Repair(
		ctx context.Context,
		nsCtx namespace.Context,
		tr xtime.Range,
		repairer databaseShardRepairer,
	) (repair.MetadataComparisonResult, error)

	// TagsFromSeriesID returns the series tags from a series ID.
	TagsFromSeriesID(seriesID ident.ID) (ident.Tags, bool, error)
}

// namespaceIndex indexes namespace writes.
type namespaceIndex interface {
	// BlockStartForWriteTime returns the index block start
	// time for the given writeTime.
	BlockStartForWriteTime(
		writeTime time.Time,
	) xtime.UnixNano

	// WriteBatch indexes the provided entries.
	WriteBatch(
		batch *index.WriteBatch,
	) error

	// Query resolves the given query into known IDs.
	Query(
		ctx context.Context,
		query index.Query,
		opts index.QueryOptions,
	) (index.QueryResult, error)

	// AggregateQuery resolves the given query into aggregated tags.
	AggregateQuery(
		ctx context.Context,
		query index.Query,
		opts index.AggregationOptions,
	) (index.AggregateQueryResult, error)

	// Bootstrap bootstraps the index the provided segments.
	Bootstrap(
		bootstrapResults result.IndexResults,
	) error

	// BootstrapsDone returns the number of completed bootstraps.
	BootstrapsDone() uint

	// CleanupExpiredFileSets removes expired fileset files. Expiration is calcuated
	// using the provided `t` as the frame of reference.
	CleanupExpiredFileSets(t time.Time) error

	// Tick performs internal house keeping in the index, including block rotation,
	// data eviction, and so on.
	Tick(c context.Cancellable, tickStart time.Time) (namespaceIndexTickResult, error)

	// Flush performs any flushes that the index has outstanding using
	// the owned shards of the database.
	Flush(
		flush persist.IndexFlush,
		shards []databaseShard,
	) error

	// Close will release the index resources and close the index.
	Close() error
}

// namespaceIndexTickResult are details about the work performed by the namespaceIndex
// during a Tick().
type namespaceIndexTickResult struct {
	NumBlocks        int64
	NumBlocksSealed  int64
	NumBlocksEvicted int64
	NumSegments      int64
	NumTotalDocs     int64
}

// namespaceIndexInsertQueue is a queue used in-front of the indexing component
// for Writes. NB: this is an interface to allow easier unit tests in namespaceIndex.
type namespaceIndexInsertQueue interface {
	// Start starts accepting writes in the queue.
	Start() error

	// Stop stops accepting writes in the queue.
	Stop() error

	// InsertBatch inserts the provided documents to the index queue which processes
	// inserts to the index asynchronously. It executes the provided callbacks
	// based on the result of the execution. The returned wait group can be used
	// if the insert is required to be synchronous.
	InsertBatch(batch *index.WriteBatch) (*sync.WaitGroup, error)
}

// databaseBootstrapManager manages the bootstrap process.
type databaseBootstrapManager interface {
	// IsBootstrapped returns whether the database is already bootstrapped.
	IsBootstrapped() bool

	// LastBootstrapCompletionTime returns the last bootstrap completion time,
	// if any.
	LastBootstrapCompletionTime() (time.Time, bool)

	// Bootstrap performs bootstrapping for all namespaces and shards owned.
	Bootstrap() error

	// Report reports runtime information.
	Report()
}

// databaseFlushManager manages flushing in-memory data to persistent storage.
type databaseFlushManager interface {
	// Flush flushes in-memory data to persistent storage.
	Flush(tickStart time.Time, dbBootstrapStateAtTickStart DatabaseBootstrapState) error

	// LastSuccessfulSnapshotStartTime returns the start time of the last
	// successful snapshot, if any.
	LastSuccessfulSnapshotStartTime() (time.Time, bool)

	// Report reports runtime information.
	Report()
}

// databaseCleanupManager manages cleaning up persistent storage space.
type databaseCleanupManager interface {
	// Cleanup cleans up data not needed in the persistent storage.
	Cleanup(t time.Time) error

	// Report reports runtime information.
	Report()
}

// databaseFileSystemManager manages the database related filesystem activities.
type databaseFileSystemManager interface {
	// Cleanup cleans up data not needed in the persistent storage.
	Cleanup(t time.Time) error

	// Flush flushes in-memory data to persistent storage.
	Flush(t time.Time, dbBootstrapStateAtTickStart DatabaseBootstrapState) error

	// Disable disables the filesystem manager and prevents it from
	// performing file operations, returns the current file operation status.
	Disable() fileOpStatus

	// Enable enables the filesystem manager to perform file operations.
	Enable() fileOpStatus

	// Status returns the file operation status.
	Status() fileOpStatus

	// Run attempts to perform all filesystem-related operations,
	// returning true if those operations are performed, and false otherwise.
	Run(
		t time.Time,
		dbBootstrapStateAtTickStart DatabaseBootstrapState,
		runType runType,
		forceType forceType,
	) bool

	// Report reports runtime information.
	Report()

	// LastSuccessfulSnapshotStartTime returns the start time of the last
	// successful snapshot, if any.
	LastSuccessfulSnapshotStartTime() (time.Time, bool)
}

// databaseShardRepairer repairs in-memory data for a shard.
type databaseShardRepairer interface {
	// Options returns the repair options.
	Options() repair.Options

	// Repair repairs the data for a given namespace and shard.
	Repair(
		ctx context.Context,
		nsCtx namespace.Context,
		tr xtime.Range,
		shard databaseShard,
	) (repair.MetadataComparisonResult, error)
}

// databaseRepairer repairs in-memory database data.
type databaseRepairer interface {
	// Start starts the repair process.
	Start()

	// Stop stops the repair process.
	Stop()

	// Repair repairs in-memory data.
	Repair() error

	// Report reports runtime information.
	Report()
}

// databaseTickManager performs periodic ticking.
type databaseTickManager interface {
	// Tick performs maintenance operations, restarting the current
	// tick if force is true. It returns nil if a new tick has
	// completed successfully, and an error otherwise.
	Tick(forceType forceType, tickStart time.Time) error
}

// databaseMediator mediates actions among various database managers.
type databaseMediator interface {
	// Open opens the mediator.
	Open() error

	// IsBootstrapped returns whether the database is bootstrapped.
	IsBootstrapped() bool

	// LastBootstrapCompletionTime returns the last bootstrap completion time,
	// if any.
	LastBootstrapCompletionTime() (time.Time, bool)

	// Bootstrap bootstraps the database with file operations performed at the end.
	Bootstrap() error

	// DisableFileOps disables file operations.
	DisableFileOps()

	// EnableFileOps enables file operations.
	EnableFileOps()

	// Tick performs a tick.
	Tick(runType runType, forceType forceType) error

	// Repair repairs the database.
	Repair() error

	// Close closes the mediator.
	Close() error

	// Report reports runtime information.
	Report()

	// LastSuccessfulSnapshotStartTime returns the start time of the last
	// successful snapshot, if any.
	LastSuccessfulSnapshotStartTime() (time.Time, bool)
}

// databaseNamespaceWatch watches for namespace updates.
type databaseNamespaceWatch interface {
	// Start starts the namespace watch.
	Start() error

	// Stop stops the namespace watch.
	Stop() error

	// close stops the watch, and releases any held resources.
	Close() error
}

// Options represents the options for storage.
type Options interface {
	// Validate validates assumptions baked into the code.
	Validate() error

	// SetEncodingM3TSZPooled sets m3tsz encoding with pooling.
	SetEncodingM3TSZPooled() Options

	// SetClockOptions sets the clock options.
	SetClockOptions(value clock.Options) Options

	// ClockOptions returns the clock options.
	ClockOptions() clock.Options

	// SetInstrumentOptions sets the instrumentation options.
	SetInstrumentOptions(value instrument.Options) Options

	// InstrumentOptions returns the instrumentation options.
	InstrumentOptions() instrument.Options

	// SetNamespaceInitializer sets the namespace registry initializer.
	SetNamespaceInitializer(value namespace.Initializer) Options

	// NamespaceInitializer returns the namespace registry initializer.
	NamespaceInitializer() namespace.Initializer

	// SetDatabaseBlockOptions sets the database block options.
	SetDatabaseBlockOptions(value block.Options) Options

	// DatabaseBlockOptions returns the database block options.
	DatabaseBlockOptions() block.Options

	// SetCommitLogOptions sets the commit log options.
	SetCommitLogOptions(value commitlog.Options) Options

	// CommitLogOptions returns the commit log options.
	CommitLogOptions() commitlog.Options

	// SetRuntimeOptionsManager sets the runtime options manager.
	SetRuntimeOptionsManager(value runtime.OptionsManager) Options

	// RuntimeOptionsManager returns the runtime options manager.
	RuntimeOptionsManager() runtime.OptionsManager

	// SetErrorWindowForLoad sets the error window for load.
	SetErrorWindowForLoad(value time.Duration) Options

	// ErrorWindowForLoad returns the error window for load.
	ErrorWindowForLoad() time.Duration

	// SetErrorThresholdForLoad sets the error threshold for load.
	SetErrorThresholdForLoad(value int64) Options

	// ErrorThresholdForLoad returns the error threshold for load.
	ErrorThresholdForLoad() int64

	// SetIndexOptions set the indexing options.
	SetIndexOptions(value index.Options) Options

	// IndexOptions returns the indexing options.
	IndexOptions() index.Options

	// SetTruncateType sets the truncation type for the database.
	SetTruncateType(value series.TruncateType) Options

	// TruncateType returns the truncation type for the database.
	TruncateType() series.TruncateType

	// SetWriteTransformOptions sets options for transforming incoming writes
	// to the database.
	SetWriteTransformOptions(value series.WriteTransformOptions) Options

	// WriteTransformOptions returns the options for transforming incoming writes
	// to the database.
	WriteTransformOptions() series.WriteTransformOptions

	// SetRepairEnabled sets whether or not to enable the repair.
	SetRepairEnabled(b bool) Options

	// RepairEnabled returns whether the repair is enabled.
	RepairEnabled() bool

	// SetRepairOptions sets the repair options.
	SetRepairOptions(value repair.Options) Options

	// RepairOptions returns the repair options.
	RepairOptions() repair.Options

	// SetBootstrapProcessProvider sets the bootstrap process provider for the database.
	SetBootstrapProcessProvider(value bootstrap.ProcessProvider) Options

	// BootstrapProcessProvider returns the bootstrap process provider for the database.
	BootstrapProcessProvider() bootstrap.ProcessProvider

	// SetPersistManager sets the persistence manager.
	SetPersistManager(value persist.Manager) Options

	// PersistManager returns the persistence manager.
	PersistManager() persist.Manager

	// SetDatabaseBlockRetrieverManager sets the block retriever manager to
	// use when bootstrapping retrievable blocks instead of blocks
	// containing data.
	// If you don't wish to bootstrap retrievable blocks instead of
	// blocks containing data then do not set this manager.
	// You can opt into which namespace you wish to have this enabled for
	// by returning nil instead of a result when creating a new block retriever
	// for a namespace from the manager.
	SetDatabaseBlockRetrieverManager(
		value block.DatabaseBlockRetrieverManager,
	) Options

	// NewBlockRetrieverFn returns the new block retriever constructor to
	// use when bootstrapping retrievable blocks instead of blocks
	// containing data.
	DatabaseBlockRetrieverManager() block.DatabaseBlockRetrieverManager

	// SetContextPool sets the contextPool.
	SetContextPool(value context.Pool) Options

	// ContextPool returns the contextPool.
	ContextPool() context.Pool

	// SetSeriesCachePolicy sets the series cache policy.
	SetSeriesCachePolicy(value series.CachePolicy) Options

	// SeriesCachePolicy returns the series cache policy.
	SeriesCachePolicy() series.CachePolicy

	// SetSeriesOptions sets the series options.
	SetSeriesOptions(value series.Options) Options

	// SeriesOptions returns the series options.
	SeriesOptions() series.Options

	// SetDatabaseSeriesPool sets the database series pool.
	SetDatabaseSeriesPool(value series.DatabaseSeriesPool) Options

	// DatabaseSeriesPool returns the database series pool.
	DatabaseSeriesPool() series.DatabaseSeriesPool

	// SetBytesPool sets the bytesPool.
	SetBytesPool(value pool.CheckedBytesPool) Options

	// BytesPool returns the bytesPool.
	BytesPool() pool.CheckedBytesPool

	// SetEncoderPool sets the contextPool.
	SetEncoderPool(value encoding.EncoderPool) Options

	// EncoderPool returns the contextPool.
	EncoderPool() encoding.EncoderPool

	// SetSegmentReaderPool sets the contextPool.
	SetSegmentReaderPool(value xio.SegmentReaderPool) Options

	// SegmentReaderPool returns the contextPool.
	SegmentReaderPool() xio.SegmentReaderPool

	// SetReaderIteratorPool sets the readerIteratorPool.
	SetReaderIteratorPool(value encoding.ReaderIteratorPool) Options

	// ReaderIteratorPool returns the readerIteratorPool.
	ReaderIteratorPool() encoding.ReaderIteratorPool

	// SetMultiReaderIteratorPool sets the multiReaderIteratorPool.
	SetMultiReaderIteratorPool(value encoding.MultiReaderIteratorPool) Options

	// MultiReaderIteratorPool returns the multiReaderIteratorPool.
	MultiReaderIteratorPool() encoding.MultiReaderIteratorPool

	// SetIDPool sets the ID pool.
	SetIdentifierPool(value ident.Pool) Options

	// IDPool returns the ID pool.
	IdentifierPool() ident.Pool

	// SetFetchBlockMetadataResultsPool sets the fetchBlockMetadataResultsPool.
	SetFetchBlockMetadataResultsPool(value block.FetchBlockMetadataResultsPool) Options

	// FetchBlockMetadataResultsPool returns the fetchBlockMetadataResultsPool.
	FetchBlockMetadataResultsPool() block.FetchBlockMetadataResultsPool

	// SetFetchBlocksMetadataResultsPool sets the fetchBlocksMetadataResultsPool.
	SetFetchBlocksMetadataResultsPool(value block.FetchBlocksMetadataResultsPool) Options

	// FetchBlocksMetadataResultsPool returns the fetchBlocksMetadataResultsPool.
	FetchBlocksMetadataResultsPool() block.FetchBlocksMetadataResultsPool

	// SetQueryIDsWorkerPool sets the QueryIDs worker pool.
	SetQueryIDsWorkerPool(value xsync.WorkerPool) Options

	// QueryIDsWorkerPool returns the QueryIDs worker pool.
	QueryIDsWorkerPool() xsync.WorkerPool

	// SetWriteBatchPool sets the WriteBatch pool.
	SetWriteBatchPool(value *ts.WriteBatchPool) Options

	// WriteBatchPool returns the WriteBatch pool.
	WriteBatchPool() *ts.WriteBatchPool

	// SetBufferBucketPool sets the BufferBucket pool.
	SetBufferBucketPool(value *series.BufferBucketPool) Options

	// BufferBucketPool returns the BufferBucket pool.
	BufferBucketPool() *series.BufferBucketPool

	// SetBufferBucketVersionsPool sets the BufferBucketVersions pool.
	SetBufferBucketVersionsPool(value *series.BufferBucketVersionsPool) Options

	// BufferBucketVersionsPool returns the BufferBucketVersions pool.
	BufferBucketVersionsPool() *series.BufferBucketVersionsPool

	// SetSchemaRegistry sets the schema registry the database uses.
	SetSchemaRegistry(registry namespace.SchemaRegistry) Options

	// SchemaRegistry returns the schema registry the database uses.
	SchemaRegistry() namespace.SchemaRegistry

	// SetBlockLeaseManager sets the block leaser.
	SetBlockLeaseManager(leaseMgr block.LeaseManager) Options

	// BlockLeaseManager returns the block leaser.
	BlockLeaseManager() block.LeaseManager
}

// DatabaseBootstrapState stores a snapshot of the bootstrap state for all shards across all
// namespaces at a given moment in time.
type DatabaseBootstrapState struct {
	NamespaceBootstrapStates NamespaceBootstrapStates
}

// NamespaceBootstrapStates stores a snapshot of the bootstrap state for all shards across a
// number of namespaces at a given moment in time.
type NamespaceBootstrapStates map[string]ShardBootstrapStates

// ShardBootstrapStates stores a snapshot of the bootstrap state for all shards for a given
// namespace.
type ShardBootstrapStates map[uint32]BootstrapState

// BootstrapState is an enum representing the possible bootstrap states for a shard.
type BootstrapState int

const (
	// BootstrapNotStarted indicates bootstrap has not been started yet.
	BootstrapNotStarted BootstrapState = iota
	// Bootstrapping indicates bootstrap process is in progress.
	Bootstrapping
	// Bootstrapped indicates a bootstrap process has completed.
	Bootstrapped
)

type newFSMergeWithMemFn func(
	shard databaseShard,
	retriever series.QueryableBlockRetriever,
	dirtySeries *dirtySeriesMap,
	dirtySeriesToWrite map[xtime.UnixNano]*idList,
) fs.MergeWith
