package frostdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apache/arrow/go/v12/arrow/ipc"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/google/btree"
	"github.com/oklog/ulid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	"github.com/polarsignals/frostdb/dynparquet"
	schemapb "github.com/polarsignals/frostdb/gen/proto/go/frostdb/schema/v1alpha1"
	schemav2pb "github.com/polarsignals/frostdb/gen/proto/go/frostdb/schema/v1alpha2"
	tablepb "github.com/polarsignals/frostdb/gen/proto/go/frostdb/table/v1alpha1"
	walpb "github.com/polarsignals/frostdb/gen/proto/go/frostdb/wal/v1alpha1"
	"github.com/polarsignals/frostdb/query/logicalplan"
	"github.com/polarsignals/frostdb/storage"
	"github.com/polarsignals/frostdb/wal"
)

type ColumnStore struct {
	mtx                 *sync.RWMutex
	dbs                 map[string]*DB
	reg                 prometheus.Registerer
	logger              log.Logger
	tracer              trace.Tracer
	granuleSizeBytes    int64
	activeMemorySize    int64
	storagePath         string
	enableWAL           bool
	compactionConfig    *CompactionConfig
	snapshotTriggerSize int64
	metrics             metrics

	// indexDegree is the degree of the btree index (default = 2)
	indexDegree int
	// splitSize is the number of new granules that are created when granules are split (default =2)
	splitSize int

	sources []DataSource
	sinks   []DataSink

	// testingOptions are options only used for testing purposes.
	testingOptions struct {
		disableReclaimDiskSpaceOnSnapshot bool
	}
}

type metrics struct {
	shutdownDuration  prometheus.Histogram
	shutdownStarted   prometheus.Counter
	shutdownCompleted prometheus.Counter
}

type Option func(*ColumnStore) error

func New(
	options ...Option,
) (*ColumnStore, error) {
	s := &ColumnStore{
		mtx:              &sync.RWMutex{},
		dbs:              map[string]*DB{},
		reg:              prometheus.NewRegistry(),
		logger:           log.NewNopLogger(),
		tracer:           trace.NewNoopTracerProvider().Tracer(""),
		indexDegree:      2,
		splitSize:        2,
		granuleSizeBytes: 1 * 1024 * 1024,   // 1MB granule size before splitting
		activeMemorySize: 512 * 1024 * 1024, // 512MB
		compactionConfig: NewCompactionConfig(),
	}

	for _, option := range options {
		if err := option(s); err != nil {
			return nil, err
		}
	}

	s.metrics = metrics{
		shutdownDuration: promauto.With(s.reg).NewHistogram(prometheus.HistogramOpts{
			Name: "frostdb_shutdown_duration",
			Help: "time it takes for the columnarstore to complete a full shutdown.",
		}),
		shutdownStarted: promauto.With(s.reg).NewCounter(prometheus.CounterOpts{
			Name: "frostdb_shutdown_started",
			Help: "Indicates a shutdown of the columnarstore has started.",
		}),
		shutdownCompleted: promauto.With(s.reg).NewCounter(prometheus.CounterOpts{
			Name: "frostdb_shutdown_completed",
			Help: "Indicates a shutdown of the columnarstore has completed.",
		}),
	}

	if s.enableWAL && s.storagePath == "" {
		return nil, fmt.Errorf("storage path must be configured if WAL is enabled")
	}

	if err := s.recoverDBsFromStorage(context.Background()); err != nil {
		return nil, err
	}

	return s, nil
}

func WithLogger(logger log.Logger) Option {
	return func(s *ColumnStore) error {
		s.logger = logger
		return nil
	}
}

func WithTracer(tracer trace.Tracer) Option {
	return func(s *ColumnStore) error {
		s.tracer = tracer
		return nil
	}
}

func WithRegistry(reg prometheus.Registerer) Option {
	return func(s *ColumnStore) error {
		s.reg = reg
		return nil
	}
}

func WithGranuleSizeBytes(bytes int64) Option {
	return func(s *ColumnStore) error {
		s.granuleSizeBytes = bytes
		return nil
	}
}

func WithActiveMemorySize(size int64) Option {
	return func(s *ColumnStore) error {
		s.activeMemorySize = size
		return nil
	}
}

func WithIndexDegree(indexDegree int) Option {
	return func(s *ColumnStore) error {
		s.indexDegree = indexDegree
		return nil
	}
}

func WithSplitSize(size int) Option {
	return func(s *ColumnStore) error {
		s.splitSize = size
		return nil
	}
}

func WithReadWriteStorage(ds DataSinkSource) Option {
	return func(s *ColumnStore) error {
		s.sources = append(s.sources, ds)
		s.sinks = append(s.sinks, ds)
		return nil
	}
}

func WithReadOnlyStorage(ds DataSource) Option {
	return func(s *ColumnStore) error {
		s.sources = append(s.sources, ds)
		return nil
	}
}

func WithWriteOnlyStorage(ds DataSink) Option {
	return func(s *ColumnStore) error {
		s.sinks = append(s.sinks, ds)
		return nil
	}
}

func WithWAL() Option {
	return func(s *ColumnStore) error {
		s.enableWAL = true
		return nil
	}
}

func WithStoragePath(path string) Option {
	return func(s *ColumnStore) error {
		s.storagePath = path
		return nil
	}
}

func WithCompactionConfig(c *CompactionConfig) Option {
	return func(s *ColumnStore) error {
		s.compactionConfig = c
		return nil
	}
}

// WithSnapshotTriggerSize specifies a table block size in bytes that will
// trigger a snapshot of the whole database. This should be less than the active
// memory size. If 0, snapshots are disabled. Note that snapshots (if enabled)
// are also triggered on block rotation of any database table.
// Snapshots are complementary to the WAL and will also be disabled if the WAL
// is disabled.
func WithSnapshotTriggerSize(size int64) Option {
	return func(s *ColumnStore) error {
		s.snapshotTriggerSize = size
		return nil
	}
}

// Close persists all data from the columnstore to storage.
// It is no longer valid to use the coumnstore for reads or writes, and the object should not longer be reused.
func (s *ColumnStore) Close() error {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	s.metrics.shutdownStarted.Inc()
	defer s.metrics.shutdownCompleted.Inc()
	defer func(ts time.Time) {
		s.metrics.shutdownDuration.Observe(float64(time.Since(ts)))
	}(time.Now())

	errg := &errgroup.Group{}
	errg.SetLimit(runtime.GOMAXPROCS(0))
	for _, db := range s.dbs {
		toClose := db
		errg.Go(func() error {
			err := toClose.Close()
			if err != nil {
				level.Error(s.logger).Log("msg", "error closing DB", "db", toClose.name, "err", err)
			}
			return err
		})
	}

	return errg.Wait()
}

func (s *ColumnStore) DatabasesDir() string {
	return filepath.Join(s.storagePath, "databases")
}

// recoverDBsFromStorage replays the snapshots and write-ahead logs for each database.
func (s *ColumnStore) recoverDBsFromStorage(ctx context.Context) error {
	if !s.enableWAL {
		return nil
	}

	dir := s.DatabasesDir()
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			level.Debug(s.logger).Log("msg", "WAL directory does not exist, no WAL to replay")
			return nil
		}
		return err
	}

	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	g, ctx := errgroup.WithContext(ctx)
	for _, f := range files {
		databaseName := f.Name()
		g.Go(func() error {
			// Open the DB for the side effect of the snapshot and WALs being loaded as part of the open operation.
			_, err := s.DB(ctx, databaseName)
			return err
		})
	}

	return g.Wait()
}

type dbMetrics struct {
	txHighWatermark prometheus.GaugeFunc
	snapshotMetrics *snapshotMetrics
}

type DB struct {
	columnStore *ColumnStore
	reg         prometheus.Registerer
	logger      log.Logger
	tracer      trace.Tracer
	name        string

	mtx      *sync.RWMutex
	roTables map[string]*Table
	tables   map[string]*Table

	storagePath string
	wal         WAL

	// The database supports multiple data sources and sinks.
	sources []DataSource
	sinks   []DataSink

	// Databases monotonically increasing transaction id
	tx *atomic.Uint64

	// TxPool is a waiting area for finished transactions that haven't been added to the watermark
	txPool *TxPool

	compactorPool *compactorPool

	// highWatermark maintains the highest consecutively completed tx number
	highWatermark *atomic.Uint64

	snapshotInProgress atomic.Bool

	metrics *dbMetrics
}

// DataSinkSource is a convenience interface for a data source and sink.
type DataSinkSource interface {
	DataSink
	DataSource
}

// DataSource is remote source of data that can be queried.
type DataSource interface {
	fmt.Stringer
	Scan(ctx context.Context, prefix string, schema *dynparquet.Schema, filter logicalplan.Expr, lastBlockTimestamp uint64, callback func(context.Context, any) error) error
	Prefixes(ctx context.Context, prefix string) ([]string, error)
}

// Datasink is a remote destination for data.
type DataSink interface {
	fmt.Stringer
	storage.Bucket
}

func (s *ColumnStore) DB(ctx context.Context, name string) (*DB, error) {
	if !validateName(name) {
		return nil, errors.New("invalid database name")
	}
	s.mtx.RLock()
	db, ok := s.dbs[name]
	s.mtx.RUnlock()
	if ok {
		return db, nil
	}

	s.mtx.Lock()
	defer s.mtx.Unlock()

	// Need to double-check that in the meantime a database with the same name
	// wasn't concurrently created.
	db, ok = s.dbs[name]
	if ok {
		return db, nil
	}

	reg := prometheus.WrapRegistererWith(prometheus.Labels{"db": name}, s.reg)
	logger := log.WithPrefix(s.logger, "db", name)
	db = &DB{
		columnStore:   s,
		name:          name,
		mtx:           &sync.RWMutex{},
		tables:        map[string]*Table{},
		roTables:      map[string]*Table{},
		reg:           reg,
		logger:        logger,
		tracer:        s.tracer,
		tx:            &atomic.Uint64{},
		highWatermark: &atomic.Uint64{},
		storagePath:   filepath.Join(s.DatabasesDir(), name),
		wal:           &wal.NopWAL{},
		sources:       s.sources,
		sinks:         s.sinks,
	}

	if dbSetupErr := func() error {
		if err := os.RemoveAll(db.trashDir()); err != nil {
			return err
		}
		db.txPool = NewTxPool(db.highWatermark)
		db.compactorPool = newCompactorPool(db, s.compactionConfig)
		if len(db.sources) != 0 {
			for _, source := range db.sources {
				prefixes, err := source.Prefixes(ctx, name)
				if err != nil {
					return err
				}

				for _, prefix := range prefixes {
					_, err := db.readOnlyTable(prefix)
					if err != nil {
						return err
					}
				}
			}
		}

		if s.enableWAL {
			var err error
			db.wal, err = db.openWAL(ctx)
			if err != nil {
				return err
			}
			// WAL pointers of tables need to be updated to the DB WAL since
			// they are loaded from object storage and snapshots with a no-op
			// WAL by default.
			for _, table := range db.tables {
				if !table.config.DisableWal {
					table.wal = db.wal
				}
			}
			for _, table := range db.roTables {
				if !table.config.DisableWal {
					table.wal = db.wal
				}
			}
		}

		// Register metrics last to avoid duplicate registration should and of the WAL or storage replay errors occur
		db.metrics = &dbMetrics{
			txHighWatermark: promauto.With(reg).NewGaugeFunc(prometheus.GaugeOpts{
				Name: "frostdb_tx_high_watermark",
				Help: "The highest transaction number that has been released to be read",
			}, func() float64 {
				return float64(db.highWatermark.Load())
			}),
		}
		if db.columnStore.snapshotTriggerSize != 0 {
			db.metrics.snapshotMetrics = newSnapshotMetrics(reg)
		}
		return nil
	}(); dbSetupErr != nil {
		// closeInternal handles closing partially set fields in the db without
		// rotating blocks etc... that the public Close method does.
		_ = db.closeInternal()
		return nil, dbSetupErr
	}

	db.compactorPool.start()
	s.dbs[name] = db
	return db, nil
}

func (db *DB) openWAL(ctx context.Context) (WAL, error) {
	wal, err := wal.Open(
		db.logger,
		db.reg,
		db.walDir(),
	)
	if err != nil {
		return nil, err
	}

	if err := db.recover(ctx, wal); err != nil {
		return nil, err
	}

	wal.RunAsync()
	return wal, nil
}

const (
	walPath       = "wal"
	snapshotsPath = "snapshots"
)

func (db *DB) walDir() string {
	return filepath.Join(db.storagePath, walPath)
}

func (db *DB) snapshotsDir() string {
	return filepath.Join(db.storagePath, snapshotsPath)
}

func (db *DB) trashDir() string {
	return filepath.Join(db.storagePath, "trash")
}

// recover attempts to recover database state from a combination of snapshots
// and the WAL.
func (db *DB) recover(ctx context.Context, wal WAL) error {
	level.Info(db.logger).Log(
		"msg", "recovering db",
		"name", db.name,
	)
	snapshotLoadStart := time.Now()
	snapshotTx, err := db.loadLatestSnapshot(ctx)
	if err != nil {
		level.Info(db.logger).Log(
			"msg", "failed to load latest snapshot", "db", db.name, "err", err,
		)
		snapshotTx = 0
	}
	firstIndex, err := wal.FirstIndex()
	if err != nil {
		level.Info(db.logger).Log(
			"msg", "failed to get WAL first index",
			"err", err)
	}
	lastIndex, err := wal.LastIndex()
	if err != nil {
		level.Info(db.logger).Log(
			"msg", "failed to get WAL last index",
			"err", err)
	}
	snapshotLogArgs := make([]any, 0)
	if snapshotTx != 0 {
		snapshotLogArgs = append(
			snapshotLogArgs,
			"snapshot_tx", snapshotTx,
			"snapshot_load_duration", time.Since(snapshotLoadStart),
		)
		if err := db.truncateSnapshotsLessThanTX(ctx, snapshotTx); err != nil {
			// Truncation is best-effort. If it fails, move on.
			level.Info(db.logger).Log(
				"msg", "failed to truncate snapshots less than loaded snapshot",
				"err", err,
				"snapshot_tx", snapshotTx,
			)
		}
		if snapshotTx > firstIndex && snapshotTx <= lastIndex {
			if err := wal.Truncate(snapshotTx); err != nil {
				// Since this is a best-effort truncation, move on if there is an
				// error.
				level.Info(db.logger).Log(
					"msg", "WAL truncation after successful snapshot load encountered error",
					"err", err,
					"first_index", firstIndex,
					"last_index", lastIndex,
					"snapshot_tx", snapshotTx,
				)
			} else {
				snapshotLogArgs = append(
					snapshotLogArgs,
					"wal_truncated", "true",
					"wal_first_index_pre_truncation", firstIndex)
				firstIndex = snapshotTx
			}
		} else {
			snapshotLogArgs = append(
				snapshotLogArgs,
				"wal_truncated", "false",
			)
		}
	}

	// persistedTables is a map from a table name to the last transaction
	// persisted.
	persistedTables := map[string]uint64{}
	lastTx := uint64(0)

	start := time.Now()
	if err := wal.Replay(snapshotTx, func(tx uint64, record *walpb.Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch e := record.Entry.EntryType.(type) {
		case *walpb.Entry_TableBlockPersisted_:
			persistedTables[e.TableBlockPersisted.TableName] = tx
			if tx > snapshotTx {
				// The loaded snapshot has data in a table that has been
				// persisted. Delete all data in this table, since it has
				// already been persisted.
				db.mtx.Lock()
				if table, ok := db.tables[e.TableBlockPersisted.TableName]; ok {
					table.ActiveBlock().index.Store(btree.New(table.db.columnStore.indexDegree))
				}
				db.mtx.Unlock()
			}
			return nil
		default:
			return nil
		}
	}); err != nil {
		return err
	}

	if err := wal.Replay(snapshotTx, func(tx uint64, record *walpb.Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastTx = tx
		switch e := record.Entry.EntryType.(type) {
		case *walpb.Entry_NewTableBlock_:
			entry := e.NewTableBlock
			var schema proto.Message
			switch v := entry.Config.Schema.(type) {
			case *tablepb.TableConfig_DeprecatedSchema:
				schema = v.DeprecatedSchema
			case *tablepb.TableConfig_SchemaV2:
				schema = v.SchemaV2
			default:
				return fmt.Errorf("unhandled schema type: %T", v)
			}

			var id ulid.ULID
			if err := id.UnmarshalBinary(entry.BlockId); err != nil {
				return err
			}

			if lastPersistedTx, ok := persistedTables[entry.TableName]; ok && tx < lastPersistedTx {
				// This block has already been successfully persisted, so we can
				// skip it.
				return nil
			}

			tableName := entry.TableName
			table, err := db.GetTable(tableName)
			var tableErr ErrTableNotFound
			if errors.As(err, &tableErr) {
				return func() error {
					db.mtx.Lock()
					defer db.mtx.Unlock()
					config := NewTableConfig(schema, FromConfig(entry.Config))
					if _, ok := db.roTables[tableName]; ok {
						table, err = db.promoteReadOnlyTableLocked(tableName, config)
						if err != nil {
							return fmt.Errorf("promoting read only table: %w", err)
						}
					} else {
						table, err = newTable(
							db,
							tableName,
							config,
							db.reg,
							db.logger,
							db.tracer,
							wal,
						)
						if err != nil {
							return fmt.Errorf("instantiate table: %w", err)
						}
					}

					table.active, err = newTableBlock(table, 0, tx, id)
					if err != nil {
						return err
					}
					db.tables[tableName] = table
					return nil
				}()
			}
			if err != nil {
				return fmt.Errorf("get table: %w", err)
			}

			// If we get to this point it means a block was finished but did
			// not get persisted.
			table.pendingBlocks[table.active] = struct{}{}
			go table.writeBlock(table.active, false /* snapshotDB */)

			protoEqual := false
			switch schema.(type) {
			case *schemav2pb.Schema:
				protoEqual = proto.Equal(schema, table.config.GetSchemaV2())
			case *schemapb.Schema:
				protoEqual = proto.Equal(schema, table.config.GetDeprecatedSchema())
			}
			if !protoEqual {
				// If schemas are identical from block to block we should we
				// reuse the previous schema in order to retain pooled memory
				// for it.
				schema, err := dynparquet.SchemaFromDefinition(schema)
				if err != nil {
					return fmt.Errorf("initialize schema: %w", err)
				}

				table.schema = schema
			}

			table.active, err = newTableBlock(table, table.active.minTx, tx, id)
			if err != nil {
				return err
			}
		case *walpb.Entry_Write_:
			entry := e.Write
			tableName := entry.TableName
			if lastPersistedTx, ok := persistedTables[tableName]; ok && tx < lastPersistedTx {
				// This write has already been successfully persisted, so we can
				// skip it.
				return nil
			}

			table, err := db.GetTable(tableName)
			var tableErr ErrTableNotFound
			if errors.As(err, &tableErr) {
				// This means the WAL was truncated at a point where this write
				// was already successfully persisted to disk in more optimized
				// form than the WAL.
				return nil
			}
			if err != nil {
				return fmt.Errorf("get table: %w", err)
			}

			switch e.Write.Arrow {
			case true:
				reader, err := ipc.NewReader(bytes.NewReader(entry.Data))
				if err != nil {
					return fmt.Errorf("create ipc reader: %w", err)
				}
				record, err := reader.Read()
				if err != nil {
					return fmt.Errorf("read record: %w", err)
				}

				if err := table.active.InsertRecord(ctx, tx, record); err != nil {
					return fmt.Errorf("insert record into block: %w", err)
				}
			default:
				serBuf, err := dynparquet.ReaderFromBytes(entry.Data)
				if err != nil {
					return fmt.Errorf("deserialize buffer: %w", err)
				}

				if err := table.active.Insert(ctx, tx, serBuf); err != nil {
					return fmt.Errorf("insert buffer into block: %w", err)
				}
			}

			// After every insert we're setting the tx and highWatermark to the replayed tx.
			// This allows the block's compaction to start working on the inserted data.
			db.tx.Store(tx)
			db.highWatermark.Store(tx)
		case *walpb.Entry_TableBlockPersisted_:
			return nil
		case *walpb.Entry_Snapshot_:
			return nil
		default:
			return fmt.Errorf("unexpected WAL entry type: %t", e)
		}
		return nil
	}); err != nil {
		return err
	}

	if lastTx >= snapshotTx {
		db.tx.Store(lastTx)
		db.highWatermark.Store(lastTx)
	}
	if lastTxn := db.tx.Load(); lastTxn != lastIndex {
		level.Warn(db.logger).Log(
			"msg", "WAL last index is != db last txn, won't be able to log records to WAL",
			"wal_last_index", lastIndex,
			"last_tx", lastTxn,
		)
	}

	level.Info(db.logger).Log(
		append(
			[]any{
				"msg", "db recovered",
				"wal_first_index", firstIndex,
				"wal_last_index", lastIndex,
				"wal_replay_duration", time.Since(start),
			},
			snapshotLogArgs...,
		)...,
	)
	return nil
}

func (db *DB) Close() error {
	level.Info(db.logger).Log("msg", "closing DB")
	for _, table := range db.tables {
		table.close()
		if len(db.sinks) != 0 {
			// Write the blocks but no snapshots since they are long-running
			// jobs.
			table.writeBlock(table.ActiveBlock(), false /* snapshotDB */)
		}
	}
	level.Info(db.logger).Log("msg", "closed all tables")

	if err := db.closeInternal(); err != nil {
		return err
	}

	if len(db.sinks) != 0 {
		// If we've successfully persisted all the table blocks we can remove
		// the wal and snapshots. Move the two directories to a trash dir since
		// it is an O(1) operation. The trash dir is cleaned up on startup in
		// case we cannot delete the WAL/snapshots in time.
		trashDir := db.trashDir()

		if moveErr := func() error {
			_ = os.Mkdir(trashDir, os.FileMode(0o755))
			// Create a temporary directory in the trash dir to avoid clashing
			// with other wal/snapshot dirs that might not have been removed
			// previously.
			tmpPath, err := os.MkdirTemp(trashDir, "")
			if err != nil {
				return err
			}
			if err := os.Rename(db.snapshotsDir(), filepath.Join(tmpPath, snapshotsPath)); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := os.Rename(db.walDir(), filepath.Join(tmpPath, walPath)); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}(); moveErr != nil {
			// If we failed to move the wal/snapshots to the trash dir, fall
			// back to attempting to remove them with RemoveAll.
			if err := os.RemoveAll(db.snapshotsDir()); err != nil {
				return fmt.Errorf("%v: %v", moveErr, err)
			}
			if err := os.RemoveAll(db.walDir()); err != nil {
				return fmt.Errorf("%v: %v", moveErr, err)
			}
			return moveErr
		}
		if err := os.RemoveAll(trashDir); err != nil {
			return err
		}
		level.Info(db.logger).Log("msg", "cleaned up wal & snapshots")
	}
	return nil
}

func (db *DB) closeInternal() error {
	if db.columnStore.enableWAL && db.wal != nil {
		if err := db.wal.Close(); err != nil {
			return err
		}
	}
	if db.txPool != nil {
		db.txPool.Stop()
	}

	if db.compactorPool != nil {
		db.compactorPool.stop()
	}

	return nil
}

func (db *DB) maintainWAL() {
	if minTx := db.getMinTXPersisted(); minTx > 0 {
		if err := db.wal.Truncate(minTx); err != nil {
			return
		}
	}
}

// reclaimDiskSpace attempts to read the latest valid snapshot txn and removes
// any snapshots/wal entries that are older than the snapshot tx.
func (db *DB) reclaimDiskSpace(ctx context.Context) error {
	if db.columnStore.testingOptions.disableReclaimDiskSpaceOnSnapshot {
		return nil
	}
	validSnapshotTxn, err := db.getLatestValidSnapshotTxn(ctx)
	if err != nil {
		return err
	}
	if validSnapshotTxn == 0 {
		return nil
	}
	if err := db.truncateSnapshotsLessThanTX(ctx, validSnapshotTxn); err != nil {
		return err
	}
	return db.wal.Truncate(validSnapshotTxn)
}

func (db *DB) getMinTXPersisted() uint64 {
	db.mtx.RLock()
	defer db.mtx.RUnlock()
	minTx := uint64(math.MaxUint64)
	for _, table := range db.tables {
		table.mtx.RLock()
		tableMinTxPersisted := table.lastCompleted
		table.mtx.RUnlock()
		if tableMinTxPersisted < minTx {
			minTx = tableMinTxPersisted
		}
	}
	return minTx
}

func (db *DB) readOnlyTable(name string) (*Table, error) {
	table, ok := db.tables[name]
	if ok {
		return table, nil
	}

	table, err := newTable(
		db,
		name,
		nil,
		db.reg,
		db.logger,
		db.tracer,
		db.wal,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create table: %w", err)
	}

	db.roTables[name] = table
	return table, nil
}

// promoteReadOnlyTableLocked promotes a read-only table to a read-write table.
// The read-write table is returned but not added to the database. Callers must
// do so.
// db.mtx must be held while calling this method.
func (db *DB) promoteReadOnlyTableLocked(name string, config *tablepb.TableConfig) (*Table, error) {
	table, ok := db.roTables[name]
	if !ok {
		return nil, fmt.Errorf("read only table %s not found", name)
	}
	schema, err := schemaFromTableConfig(config)
	if err != nil {
		return nil, err
	}
	table.config = config
	table.schema = schema
	delete(db.roTables, name)
	return table, nil
}

func (db *DB) Table(name string, config *tablepb.TableConfig) (*Table, error) {
	if !validateName(name) {
		return nil, errors.New("invalid table name")
	}
	db.mtx.RLock()
	table, ok := db.tables[name]
	db.mtx.RUnlock()
	if ok {
		return table, nil
	}

	db.mtx.Lock()
	defer db.mtx.Unlock()

	// Need to double-check that in the meantime another table with the same
	// name wasn't concurrently created.
	table, ok = db.tables[name]
	if ok {
		return table, nil
	}

	// Check if this table exists as a read only table
	if _, ok := db.roTables[name]; ok {
		var err error
		table, err = db.promoteReadOnlyTableLocked(name, config)
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		table, err = newTable(
			db,
			name,
			config,
			db.reg,
			db.logger,
			db.tracer,
			db.wal,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create table: %w", err)
		}
	}

	tx, _, commit := db.begin()
	defer commit()

	id := generateULID()
	if err := table.newTableBlock(0, tx, id); err != nil {
		return nil, err
	}

	db.tables[name] = table
	return table, nil
}

type ErrTableNotFound struct {
	TableName string
}

func (e ErrTableNotFound) Error() string {
	return fmt.Sprintf("table %s not found", e.TableName)
}

func (db *DB) GetTable(name string) (*Table, error) {
	db.mtx.RLock()
	table, ok := db.tables[name]
	db.mtx.RUnlock()
	if !ok {
		return nil, ErrTableNotFound{TableName: name}
	}
	return table, nil
}

func (db *DB) TableProvider() *DBTableProvider {
	return NewDBTableProvider(db)
}

type DBTableProvider struct {
	db *DB
}

func NewDBTableProvider(db *DB) *DBTableProvider {
	return &DBTableProvider{
		db: db,
	}
}

func (p *DBTableProvider) GetTable(name string) (logicalplan.TableReader, error) {
	p.db.mtx.RLock()
	defer p.db.mtx.RUnlock()
	tbl, ok := p.db.tables[name]
	if ok {
		return tbl, nil
	}

	tbl, ok = p.db.roTables[name]
	if ok {
		return tbl, nil
	}

	return nil, fmt.Errorf("table %v not found", name)
}

// beginRead returns the high watermark. Reads can safely access any write that has a lower or equal tx id than the returned number.
func (db *DB) beginRead() uint64 {
	return db.highWatermark.Load()
}

// begin is an internal function that Tables call to start a transaction for writes.
// It returns:
//
//	the write tx id
//	The current high watermark
//	A function to complete the transaction
func (db *DB) begin() (uint64, uint64, func()) {
	tx := db.tx.Add(1)
	watermark := db.highWatermark.Load()
	return tx, watermark, func() {
		if mark := db.highWatermark.Load(); mark+1 == tx { // This is the next consecutive transaction; increate the watermark
			db.highWatermark.Add(1)
		}

		// place completed transaction in the waiting pool
		db.txPool.Insert(tx)
	}
}

// Wait is a blocking function that returns once the high watermark has equaled or exceeded the transaction id.
// Wait makes no differentiation between completed and aborted transactions.
func (db *DB) Wait(tx uint64) {
	for {
		if db.highWatermark.Load() >= tx {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// validateName ensures that the passed in name doesn't violate any constrainsts.
func validateName(name string) bool {
	return !strings.Contains(name, "/")
}
