// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	uuid "github.com/google/uuid"
	"github.com/pingcap/errors"
	sst "github.com/pingcap/kvproto/pkg/import_sstpb"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/log"
	"github.com/pingcap/parser/model"
	filter "github.com/pingcap/tidb-tools/pkg/table-filter"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/meta/autoid"
	"github.com/pingcap/tidb/store/tikv/oracle"
	titable "github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/util/codec"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/pingcap/br/pkg/cdclog"
	berrors "github.com/pingcap/br/pkg/errors"
	"github.com/pingcap/br/pkg/kv"
	"github.com/pingcap/br/pkg/storage"
	"github.com/pingcap/br/pkg/utils"
)

const (
	tableLogPrefix = "t_"
	logPrefix      = "cdclog"

	metaFile      = "log.meta"
	ddlEventsDir  = "ddls"
	ddlFilePrefix = "ddl"

	maxUint64 = ^uint64(0)

	maxRetryTimes = 3
)

// concurrencyCfg set by user, which can adjust the restore performance.
type concurrencyCfg struct {
	BatchWriteKVPairs int
	BatchFlushKVPairs int
	BatchFlushKVSize  int64
	Concurrency       uint
}

// LogMeta represents the log.meta generated by cdc log backup.
type LogMeta struct {
	Names            map[int64]string `json:"names"`
	GlobalResolvedTS uint64           `json:"global_resolved_ts"`
}

// LogClient sends requests to restore files.
type LogClient struct {
	// lock DDL execution
	// TODO remove lock by using db session pool if necessary
	ddlLock sync.Mutex

	restoreClient  *Client
	splitClient    SplitClient
	importerClient ImporterClient

	// range of log backup
	startTs uint64
	endTs   uint64

	concurrencyCfg concurrencyCfg
	// meta info parsed from log backup
	meta         *LogMeta
	eventPullers map[int64]*cdclog.EventPuller
	tableBuffers map[int64]*cdclog.TableBuffer

	tableFilter filter.Filter

	// a map to store all drop schema ts, use it as a filter
	dropTSMap sync.Map
}

// NewLogRestoreClient returns a new LogRestoreClient.
func NewLogRestoreClient(
	ctx context.Context,
	restoreClient *Client,
	startTs uint64,
	endTs uint64,
	tableFilter filter.Filter,
	concurrency uint,
	batchFlushPairs int,
	batchFlushSize int64,
	batchWriteKVPairs int,
) (*LogClient, error) {
	var err error
	if endTs == 0 {
		// means restore all log data,
		// so we get current ts from restore cluster
		endTs, err = restoreClient.GetTS(ctx)
		if err != nil {
			return nil, err
		}
	}

	splitClient := NewSplitClient(restoreClient.GetPDClient(), restoreClient.GetTLSConfig())
	importClient := NewImportClient(splitClient, restoreClient.tlsConf)

	cfg := concurrencyCfg{
		Concurrency:       concurrency,
		BatchFlushKVPairs: batchFlushPairs,
		BatchFlushKVSize:  batchFlushSize,
		BatchWriteKVPairs: batchWriteKVPairs,
	}

	lc := &LogClient{
		restoreClient:  restoreClient,
		splitClient:    splitClient,
		importerClient: importClient,
		startTs:        startTs,
		endTs:          endTs,
		concurrencyCfg: cfg,
		meta:           new(LogMeta),
		eventPullers:   make(map[int64]*cdclog.EventPuller),
		tableBuffers:   make(map[int64]*cdclog.TableBuffer),
		tableFilter:    tableFilter,
	}
	return lc, nil
}

func (l *LogClient) tsInRange(ts uint64) bool {
	return l.startTs <= ts && ts <= l.endTs
}

func (l *LogClient) shouldFilter(item *cdclog.SortItem) bool {
	if val, ok := l.dropTSMap.Load(item.Schema); ok {
		if val.(uint64) > item.TS {
			return true
		}
	}
	return false
}

func (l *LogClient) needRestoreDDL(fileName string) (bool, error) {
	names := strings.Split(fileName, ".")
	if len(names) != 2 {
		log.Warn("found wrong format of ddl file", zap.String("file", fileName))
		return false, nil
	}
	if names[0] != ddlFilePrefix {
		log.Warn("file doesn't start with ddl", zap.String("file", fileName))
		return false, nil
	}
	ts, err := strconv.ParseUint(names[1], 10, 64)
	if err != nil {
		return false, errors.Trace(err)
	}

	// According to https://docs.aws.amazon.com/AmazonS3/latest/dev/ListingKeysUsingAPIs.html
	// list API return in UTF-8 binary order, so the cdc log create DDL file used
	// maxUint64 - the first DDL event's commit ts as the file name to return the latest ddl file.
	// see details at https://github.com/pingcap/ticdc/pull/826/files#diff-d2e98b3ed211b7b9bb7b6da63dd48758R81
	ts = maxUint64 - ts
	if l.tsInRange(ts) {
		return true, nil
	}
	log.Info("filter ddl file by ts", zap.String("name", fileName), zap.Uint64("ts", ts))
	return false, nil
}

func (l *LogClient) collectDDLFiles(ctx context.Context) ([]string, error) {
	ddlFiles := make([]string, 0)
	opt := &storage.WalkOption{
		SubDir:    ddlEventsDir,
		ListCount: -1,
	}
	err := l.restoreClient.storage.WalkDir(ctx, opt, func(path string, size int64) error {
		fileName := filepath.Base(path)
		shouldRestore, err := l.needRestoreDDL(fileName)
		if err != nil {
			return err
		}
		if shouldRestore {
			ddlFiles = append(ddlFiles, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Sort(sort.Reverse(sort.StringSlice(ddlFiles)))
	return ddlFiles, nil
}

func (l *LogClient) isDBRelatedDDL(ddl *cdclog.MessageDDL) bool {
	switch ddl.Type {
	case model.ActionDropSchema, model.ActionCreateSchema, model.ActionModifySchemaCharsetAndCollate:
		return true
	}
	return false
}

func (l *LogClient) doDBDDLJob(ctx context.Context, ddls []string) error {
	if len(ddls) == 0 {
		log.Info("no ddls to restore")
		return nil
	}

	for _, path := range ddls {
		data, err := l.restoreClient.storage.Read(ctx, path)
		if err != nil {
			return errors.Trace(err)
		}
		eventDecoder, err := cdclog.NewJSONEventBatchDecoder(data)
		if err != nil {
			return errors.Trace(err)
		}
		for eventDecoder.HasNext() {
			item, err := eventDecoder.NextEvent(cdclog.DDL)
			if err != nil {
				return errors.Trace(err)
			}
			ddl := item.Data.(*cdclog.MessageDDL)
			log.Debug("[doDBDDLJob] parse ddl", zap.String("query", ddl.Query))
			if l.isDBRelatedDDL(ddl) && l.tsInRange(item.TS) {
				err = l.restoreClient.db.se.Execute(ctx, ddl.Query)
				if err != nil {
					log.Error("[doDBDDLJob] exec ddl failed",
						zap.String("query", ddl.Query),
						zap.Error(err))
					return errors.Trace(err)
				}
				if ddl.Type == model.ActionDropSchema {
					// store the drop schema ts, and then we need filter evetns which ts is small than this.
					l.dropTSMap.Store(item.Schema, item.TS)
				}
			}
		}
	}
	return nil
}

func (l *LogClient) needRestoreRowChange(fileName string) (bool, error) {
	if fileName == logPrefix {
		// this file name appeared when file sink enabled
		return true, nil
	}
	names := strings.Split(fileName, ".")
	if len(names) != 2 {
		log.Warn("found wrong format of row changes file", zap.String("file", fileName))
		return false, nil
	}
	if names[0] != logPrefix {
		log.Warn("file doesn't start with row changes file", zap.String("file", fileName))
		return false, nil
	}
	ts, err := strconv.ParseUint(names[1], 10, 64)
	if err != nil {
		return false, errors.Trace(err)
	}
	if l.tsInRange(ts) {
		return true, nil
	}
	log.Info("filter file by ts", zap.String("name", fileName), zap.Uint64("ts", ts))
	return false, nil
}

func (l *LogClient) collectRowChangeFiles(ctx context.Context) (map[int64][]string, error) {
	// we should collect all related tables row change files
	// by log meta info and by given table filter
	rowChangeFiles := make(map[int64][]string)

	// need collect restore tableIDs
	tableIDs := make([]int64, 0, len(l.meta.Names))
	for tableID, name := range l.meta.Names {
		schema, table := ParseQuoteName(name)
		if !l.tableFilter.MatchTable(schema, table) {
			log.Info("filter tables",
				zap.String("schema", schema),
				zap.String("table", table),
				zap.Int64("tableID", tableID),
			)
			continue
		}
		tableIDs = append(tableIDs, tableID)
	}

	for _, tID := range tableIDs {
		tableID := tID
		// FIXME update log meta logic here
		dir := fmt.Sprintf("%s%d", tableLogPrefix, tableID)
		opt := &storage.WalkOption{
			SubDir:    dir,
			ListCount: -1,
		}
		err := l.restoreClient.storage.WalkDir(ctx, opt, func(path string, size int64) error {
			fileName := filepath.Base(path)
			shouldRestore, err := l.needRestoreRowChange(fileName)
			if err != nil {
				return err
			}
			if shouldRestore {
				if _, ok := rowChangeFiles[tableID]; ok {
					rowChangeFiles[tableID] = append(rowChangeFiles[tableID], path)
				} else {
					rowChangeFiles[tableID] = []string{path}
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	// sort file in order
	for tID, files := range rowChangeFiles {
		sortFiles := files
		sort.Slice(sortFiles, func(i, j int) bool {
			if filepath.Base(sortFiles[j]) == logPrefix {
				return true
			}
			return sortFiles[i] < sortFiles[j]
		})
		rowChangeFiles[tID] = sortFiles
	}

	return rowChangeFiles, nil
}

func (l *LogClient) writeToTiKV(ctx context.Context, kvs kv.Pairs, region *RegionInfo) ([]*sst.SSTMeta, error) {
	firstKey := codec.EncodeBytes([]byte{}, kvs[0].Key)
	lastKey := codec.EncodeBytes([]byte{}, kvs[len(kvs)-1].Key)

	uid := uuid.New()
	meta := &sst.SSTMeta{
		Uuid:        uid[:],
		RegionId:    region.Region.GetId(),
		RegionEpoch: region.Region.GetRegionEpoch(),
		Range: &sst.Range{
			Start: firstKey,
			End:   lastKey,
		},
	}

	leaderID := region.Leader.GetId()
	clients := make([]sst.ImportSST_WriteClient, 0, len(region.Region.GetPeers()))
	requests := make([]*sst.WriteRequest, 0, len(region.Region.GetPeers()))
	commitTs := oracle.ComposeTS(time.Now().Unix()*1000, 0)
	for _, peer := range region.Region.GetPeers() {
		cli, err := l.importerClient.GetImportClient(ctx, peer.StoreId)
		if err != nil {
			return nil, err
		}

		wstream, err := cli.Write(ctx)
		if err != nil {
			return nil, err
		}

		// Bind uuid for this write request
		req := &sst.WriteRequest{
			Chunk: &sst.WriteRequest_Meta{
				Meta: meta,
			},
		}
		if err = wstream.Send(req); err != nil {
			return nil, err
		}
		req.Chunk = &sst.WriteRequest_Batch{
			Batch: &sst.WriteBatch{
				// FIXME we should discuss about the commit ts
				// 1. give a batch of kv a specify commit ts
				// 2. give each kv the backup commit ts
				CommitTs: commitTs,
			},
		}
		clients = append(clients, wstream)
		requests = append(requests, req)
	}

	bytesBuf := utils.NewBytesBuffer()
	defer bytesBuf.Destroy()
	pairs := make([]*sst.Pair, 0, l.concurrencyCfg.BatchWriteKVPairs)
	count := 0
	size := int64(0)
	totalCount := 0
	firstLoop := true
	for _, kv := range kvs {
		op := sst.Pair_Put
		if kv.IsDelete {
			op = sst.Pair_Delete
		}
		size += int64(len(kv.Key) + len(kv.Val))
		// here we reuse the `*sst.Pair`s to optimize object allocation
		if firstLoop {
			pair := &sst.Pair{
				Key:   bytesBuf.AddBytes(kv.Key),
				Value: bytesBuf.AddBytes(kv.Val),
				Op:    op,
			}
			pairs = append(pairs, pair)
		} else {
			pairs[count].Key = bytesBuf.AddBytes(kv.Key)
			pairs[count].Value = bytesBuf.AddBytes(kv.Val)
			pairs[count].Op = op
		}
		count++
		totalCount++

		if count >= l.concurrencyCfg.BatchWriteKVPairs {
			for i := range clients {
				requests[i].Chunk.(*sst.WriteRequest_Batch).Batch.Pairs = pairs
				if err := clients[i].Send(requests[i]); err != nil {
					return nil, err
				}
			}
			count = 0
			bytesBuf.Reset()
			firstLoop = false
		}
	}
	if count > 0 {
		for i := range clients {
			requests[i].Chunk.(*sst.WriteRequest_Batch).Batch.Pairs = pairs[:count]
			if err := clients[i].Send(requests[i]); err != nil {
				return nil, err
			}
		}
	}

	var leaderPeerMetas []*sst.SSTMeta
	for i, wStream := range clients {
		if resp, closeErr := wStream.CloseAndRecv(); closeErr != nil {
			return nil, closeErr
		} else if leaderID == region.Region.Peers[i].GetId() {
			leaderPeerMetas = resp.Metas
			log.Debug("get metas after write kv stream to tikv", zap.Reflect("metas", leaderPeerMetas))
		}
	}

	log.Debug("write to kv", zap.Reflect("region", region), zap.Uint64("leader", leaderID),
		zap.Reflect("meta", meta), zap.Reflect("return metas", leaderPeerMetas),
		zap.Int("kv_pairs", totalCount), zap.Int64("total_bytes", size),
		zap.Int64("buf_size", bytesBuf.TotalSize()))

	return leaderPeerMetas, nil
}

// Ingest ingests sst to TiKV.
func (l *LogClient) Ingest(ctx context.Context, meta *sst.SSTMeta, region *RegionInfo) (*sst.IngestResponse, error) {
	leader := region.Leader
	if leader == nil {
		leader = region.Region.GetPeers()[0]
	}

	cli, err := l.importerClient.GetImportClient(ctx, leader.StoreId)
	if err != nil {
		return nil, err
	}
	reqCtx := &kvrpcpb.Context{
		RegionId:    region.Region.GetId(),
		RegionEpoch: region.Region.GetRegionEpoch(),
		Peer:        leader,
	}

	req := &sst.IngestRequest{
		Context: reqCtx,
		Sst:     meta,
	}
	resp, err := cli.Ingest(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (l *LogClient) doWriteAndIngest(ctx context.Context, kvs kv.Pairs, region *RegionInfo) error {
	var startKey, endKey []byte
	if len(region.Region.StartKey) > 0 {
		_, startKey, _ = codec.DecodeBytes(region.Region.StartKey, []byte{})
	}
	if len(region.Region.EndKey) > 0 {
		_, endKey, _ = codec.DecodeBytes(region.Region.EndKey, []byte{})
	}

	var start, end int
	// TODO use binary search
	for i, kv := range kvs {
		if bytes.Compare(kv.Key, startKey) >= 0 {
			start = i
			break
		}
	}
	for i := len(kvs) - 1; i >= 0; i-- {
		if beforeEnd(kvs[i].Key, endKey) {
			end = i + 1
			break
		}
	}

	log.Debug("doWriteAndIngest", zap.Int("kv count", len(kvs)),
		zap.Int("start", start), zap.Int("end", end))

	metas, err := l.writeToTiKV(ctx, kvs[start:end], region)
	if err != nil {
		log.Warn("write to tikv failed", zap.Error(err))
		return err
	}

	for _, meta := range metas {
		for i := 0; i < maxRetryTimes; i++ {
			log.Debug("ingest meta", zap.Reflect("meta", meta))
			resp, err := l.Ingest(ctx, meta, region)
			if err != nil {
				log.Warn("ingest failed", zap.Error(err), zap.Reflect("meta", meta),
					zap.Reflect("region", region))
				continue
			}
			needRetry, newRegion, errIngest := isIngestRetryable(resp, region, meta)
			if errIngest == nil {
				// ingest next meta
				break
			}
			if !needRetry {
				log.Warn("ingest failed noretry", zap.Error(errIngest), zap.Reflect("meta", meta),
					zap.Reflect("region", region))
				// met non-retryable error retry whole Write procedure
				return errIngest
			}
			// retry with not leader and epoch not match error
			if newRegion != nil && i < maxRetryTimes-1 {
				region = newRegion
			} else {
				log.Warn("retry ingest due to",
					zap.Reflect("meta", meta), zap.Reflect("region", region),
					zap.Reflect("new region", newRegion), zap.Error(errIngest))
				return errIngest
			}
		}
	}
	return nil
}

func (l *LogClient) writeAndIngestPairs(tctx context.Context, kvs kv.Pairs) error {
	var (
		regions []*RegionInfo
		err     error
	)

	pairStart := kvs[0].Key
	pairEnd := kvs[len(kvs)-1].Key

	ctx, cancel := context.WithCancel(tctx)
	defer cancel()
WriteAndIngest:
	for retry := 0; retry < maxRetryTimes; retry++ {
		if retry != 0 {
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		startKey := codec.EncodeBytes([]byte{}, pairStart)
		endKey := codec.EncodeBytes([]byte{}, nextKey(pairEnd))
		regions, err = PaginateScanRegion(ctx, l.splitClient, startKey, endKey, 128)
		if err != nil || len(regions) == 0 {
			log.Warn("scan region failed", zap.Error(err), zap.Int("region_len", len(regions)))
			continue WriteAndIngest
		}

		shouldWait := false
		eg, ectx := errgroup.WithContext(ctx)
		for _, region := range regions {
			log.Debug("get region", zap.Int("retry", retry), zap.Binary("startKey", startKey),
				zap.Binary("endKey", endKey), zap.Uint64("id", region.Region.GetId()),
				zap.Stringer("epoch", region.Region.GetRegionEpoch()), zap.Binary("start", region.Region.GetStartKey()),
				zap.Binary("end", region.Region.GetEndKey()), zap.Reflect("peers", region.Region.GetPeers()))

			// generate new uuid for concurrent write to tikv
			if len(regions) == 1 {
				if err = l.doWriteAndIngest(ctx, kvs, region); err != nil {
					continue WriteAndIngest
				}
			} else {
				shouldWait = true
				regionReplica := region
				eg.Go(func() error {
					return l.doWriteAndIngest(ectx, kvs, regionReplica)
				})
			}
		}
		if shouldWait {
			err1 := eg.Wait()
			if err1 != nil {
				err = err1
				log.Warn("should retry this range", zap.Int("retry", retry), zap.Error(err))
				continue WriteAndIngest
			}
		}
		return nil
	}
	if err == nil {
		err = errors.Annotate(berrors.ErrRestoreWriteAndIngest, "all retry failed")
	}
	return err
}

func (l *LogClient) writeRows(ctx context.Context, kvs kv.Pairs) error {
	log.Info("writeRows", zap.Int("kv count", len(kvs)))
	if len(kvs) == 0 {
		// shouldn't happen
		log.Warn("not rows to write")
		return nil
	}

	// stable sort kvs in memory
	sort.SliceStable(kvs, func(i, j int) bool {
		return bytes.Compare(kvs[i].Key, kvs[j].Key) < 0
	})

	// remove duplicate keys, and keep the last one
	newKvs := make([]kv.Pair, 0, len(kvs))
	for i := 0; i < len(kvs); i++ {
		if i == len(kvs)-1 {
			newKvs = append(newKvs, kvs[i])
			break
		}
		if bytes.Equal(kvs[i].Key, kvs[i+1].Key) {
			// skip this one
			continue
		}
		newKvs = append(newKvs, kvs[i])
	}

	return l.writeAndIngestPairs(ctx, newKvs)
}

func (l *LogClient) reloadTableMeta(dom *domain.Domain, tableID int64, item *cdclog.SortItem) error {
	err := dom.Reload()
	if err != nil {
		return errors.Trace(err)
	}
	// find tableID for this table on cluster
	newTableID := l.tableBuffers[tableID].TableID()
	var (
		newTableInfo titable.Table
		ok           bool
	)
	if newTableID != 0 {
		newTableInfo, ok = dom.InfoSchema().TableByID(newTableID)
		if !ok {
			log.Error("[restoreFromPuller] can't get table info from dom by tableID",
				zap.Int64("backup table id", tableID),
				zap.Int64("restore table id", newTableID),
			)
			return errors.Trace(err)
		}
	} else {
		// fall back to use schema table get info
		newTableInfo, err = dom.InfoSchema().TableByName(
			model.NewCIStr(item.Schema), model.NewCIStr(item.Table))
		if err != nil {
			log.Error("[restoreFromPuller] can't get table info from dom by table name",
				zap.Int64("backup table id", tableID),
				zap.Int64("restore table id", newTableID),
				zap.String("restore table name", item.Table),
				zap.String("restore schema name", item.Schema),
			)
			return errors.Trace(err)
		}
	}

	dbInfo, ok := dom.InfoSchema().SchemaByName(model.NewCIStr(item.Schema))
	if !ok {
		return errors.Annotatef(berrors.ErrRestoreSchemaNotExists, "schema %s", item.Schema)
	}
	allocs := autoid.NewAllocatorsFromTblInfo(dom.Store(), dbInfo.ID, newTableInfo.Meta())

	// reload
	l.tableBuffers[tableID].ReloadMeta(newTableInfo, allocs)
	log.Debug("reload table meta for table",
		zap.Int64("backup table id", tableID),
		zap.Int64("restore table id", newTableID),
		zap.String("restore table name", item.Table),
		zap.String("restore schema name", item.Schema),
		zap.Any("allocator", len(allocs)),
		zap.Any("auto", newTableInfo.Meta().GetAutoIncrementColInfo()),
	)
	return nil
}

func (l *LogClient) applyKVChanges(ctx context.Context, tableID int64) error {
	log.Info("apply kv changes to tikv",
		zap.Any("table", tableID),
	)
	dataKVs := kv.Pairs{}
	indexKVs := kv.Pairs{}

	tableBuffer := l.tableBuffers[tableID]
	if tableBuffer.IsEmpty() {
		log.Warn("no kv changes to apply")
		return nil
	}

	var dataChecksum, indexChecksum kv.Checksum
	for _, p := range tableBuffer.KvPairs {
		p.ClassifyAndAppend(&dataKVs, &dataChecksum, &indexKVs, &indexChecksum)
	}

	err := l.writeRows(ctx, dataKVs)
	if err != nil {
		return errors.Trace(err)
	}
	dataKVs = dataKVs.Clear()

	err = l.writeRows(ctx, indexKVs)
	if err != nil {
		return errors.Trace(err)
	}
	indexKVs = indexKVs.Clear()

	tableBuffer.Clear()

	return nil
}

func (l *LogClient) restoreTableFromPuller(
	ctx context.Context,
	tableID int64,
	puller *cdclog.EventPuller,
	dom *domain.Domain) error {
	for {
		item, err := puller.PullOneEvent(ctx)
		if err != nil {
			return errors.Trace(err)
		}
		if item == nil {
			log.Info("[restoreFromPuller] nothing in this puller, we should stop and flush",
				zap.Int64("table id", tableID))
			err := l.applyKVChanges(ctx, tableID)
			if err != nil {
				return errors.Trace(err)
			}
			return nil
		}
		log.Debug("[restoreFromPuller] next event", zap.Any("item", item), zap.Int64("table id", tableID))
		if !l.tsInRange(item.TS) {
			log.Warn("[restoreFromPuller] ts not in given range, we should stop and flush",
				zap.Uint64("start ts", l.startTs),
				zap.Uint64("end ts", l.endTs),
				zap.Uint64("item ts", item.TS),
				zap.Int64("table id", tableID))
			err := l.applyKVChanges(ctx, tableID)
			if err != nil {
				return errors.Trace(err)
			}
			return nil
		}

		if l.shouldFilter(item) {
			log.Debug("[restoreFromPuller] filter item because later drop schema will affect on this item",
				zap.Any("item", item),
				zap.Int64("table id", tableID))
			err := l.applyKVChanges(ctx, tableID)
			if err != nil {
				return errors.Trace(err)
			}
			continue
		}

		switch item.ItemType {
		case cdclog.DDL:
			name := l.meta.Names[tableID]
			schema, table := ParseQuoteName(name)
			ddl := item.Data.(*cdclog.MessageDDL)
			// ddl not influence on this schema/table
			if !(schema == item.Schema && (table == item.Table || l.isDBRelatedDDL(ddl))) {
				log.Info("[restoreFromPuller] meet unrelated ddl, and continue pulling",
					zap.String("item table", item.Table),
					zap.String("table", table),
					zap.String("item schema", item.Schema),
					zap.String("schema", schema),
					zap.Int64("backup table id", tableID),
					zap.String("query", ddl.Query),
					zap.Int64("table id", tableID))
				continue
			}

			// database level ddl job has been executed at the beginning
			if l.isDBRelatedDDL(ddl) {
				log.Debug("[restoreFromPuller] meet database level ddl, continue pulling",
					zap.String("ddl", ddl.Query),
					zap.Int64("table id", tableID))
				continue
			}

			// wait all previous kvs ingest finished
			err = l.applyKVChanges(ctx, tableID)
			if err != nil {
				return errors.Trace(err)
			}

			log.Debug("[restoreFromPuller] execute ddl", zap.String("ddl", ddl.Query))

			l.ddlLock.Lock()
			err = l.restoreClient.db.se.Execute(ctx, fmt.Sprintf("use %s", item.Schema))
			if err != nil {
				return errors.Trace(err)
			}

			err = l.restoreClient.db.se.Execute(ctx, ddl.Query)
			if err != nil {
				return errors.Trace(err)
			}
			l.ddlLock.Unlock()

			err = l.reloadTableMeta(dom, tableID, item)
			if err != nil {
				return errors.Trace(err)
			}

		case cdclog.RowChanged:
			if l.tableBuffers[tableID].TableInfo() == nil {
				err = l.reloadTableMeta(dom, tableID, item)
				if err != nil {
					// shouldn't happen
					return errors.Trace(err)
				}
			}
			err = l.tableBuffers[tableID].Append(item)
			if err != nil {
				return errors.Trace(err)
			}
			if l.tableBuffers[tableID].ShouldApply() {
				err = l.applyKVChanges(ctx, tableID)
				if err != nil {
					return errors.Trace(err)
				}
			}
		}
	}
}

func (l *LogClient) restoreTables(ctx context.Context, dom *domain.Domain) error {
	// 1. decode cdclog with in ts range
	// 2. dispatch cdclog events to table level concurrently
	// 		a. encode row changed files to kvpairs and ingest into tikv
	// 		b. exec ddl
	log.Debug("start restore tables")
	workerPool := utils.NewWorkerPool(l.concurrencyCfg.Concurrency, "table log restore")
	eg, ectx := errgroup.WithContext(ctx)
	for tableID, puller := range l.eventPullers {
		pullerReplica := puller
		tableIDReplica := tableID
		workerPool.ApplyOnErrorGroup(eg, func() error {
			return l.restoreTableFromPuller(ectx, tableIDReplica, pullerReplica, dom)
		})
	}
	return eg.Wait()
}

// RestoreLogData restore specify log data from storage.
func (l *LogClient) RestoreLogData(ctx context.Context, dom *domain.Domain) error {
	// 1. Retrieve log data from storage
	// 2. Find proper data by TS range
	// 3. Encode and ingest data to tikv

	// parse meta file
	data, err := l.restoreClient.storage.Read(ctx, metaFile)
	if err != nil {
		return errors.Trace(err)
	}
	err = json.Unmarshal(data, l.meta)
	if err != nil {
		return errors.Trace(err)
	}
	log.Info("get meta from storage", zap.Binary("data", data))

	if l.startTs > l.meta.GlobalResolvedTS {
		return errors.Annotatef(berrors.ErrRestoreRTsConstrain,
			"start ts:%d is greater than resolved ts:%d", l.startTs, l.meta.GlobalResolvedTS)
	}
	if l.endTs > l.meta.GlobalResolvedTS {
		log.Info("end ts is greater than resolved ts,"+
			" to keep consistency we only recover data until resolved ts",
			zap.Uint64("end ts", l.endTs),
			zap.Uint64("resolved ts", l.meta.GlobalResolvedTS))
		l.endTs = l.meta.GlobalResolvedTS
	}

	// collect ddl files
	ddlFiles, err := l.collectDDLFiles(ctx)
	if err != nil {
		return errors.Trace(err)
	}

	log.Info("collect ddl files", zap.Any("files", ddlFiles))

	err = l.doDBDDLJob(ctx, ddlFiles)
	if err != nil {
		return errors.Trace(err)
	}
	log.Debug("db level ddl executed")

	// collect row change files
	rowChangesFiles, err := l.collectRowChangeFiles(ctx)
	if err != nil {
		return errors.Trace(err)
	}

	log.Info("collect row changed files", zap.Any("files", rowChangesFiles))

	// create event puller to apply changes concurrently
	for tableID, files := range rowChangesFiles {
		name := l.meta.Names[tableID]
		schema, table := ParseQuoteName(name)
		log.Info("create puller for table",
			zap.Int64("table id", tableID),
			zap.String("schema", schema),
			zap.String("table", table),
		)
		l.eventPullers[tableID], err = cdclog.NewEventPuller(ctx, schema, table, ddlFiles, files, l.restoreClient.storage)
		if err != nil {
			return errors.Trace(err)
		}
		// use table name to get table info
		var tableInfo titable.Table
		var allocs autoid.Allocators
		infoSchema := dom.InfoSchema()
		if infoSchema.TableExists(model.NewCIStr(schema), model.NewCIStr(table)) {
			tableInfo, err = infoSchema.TableByName(model.NewCIStr(schema), model.NewCIStr(table))
			if err != nil {
				return errors.Trace(err)
			}
			dbInfo, ok := dom.InfoSchema().SchemaByName(model.NewCIStr(schema))
			if !ok {
				return errors.Annotatef(berrors.ErrRestoreSchemaNotExists, "schema %s", schema)
			}
			allocs = autoid.NewAllocatorsFromTblInfo(dom.Store(), dbInfo.ID, tableInfo.Meta())
		}

		l.tableBuffers[tableID] = cdclog.NewTableBuffer(tableInfo, allocs,
			l.concurrencyCfg.BatchFlushKVPairs, l.concurrencyCfg.BatchFlushKVSize)
	}
	// restore files
	return l.restoreTables(ctx, dom)
}

func isIngestRetryable(resp *sst.IngestResponse, region *RegionInfo, meta *sst.SSTMeta) (bool, *RegionInfo, error) {
	if resp.GetError() == nil {
		return false, nil, nil
	}

	var newRegion *RegionInfo
	switch errPb := resp.GetError(); {
	case errPb.NotLeader != nil:
		if newLeader := errPb.GetNotLeader().GetLeader(); newLeader != nil {
			newRegion = &RegionInfo{
				Leader: newLeader,
				Region: region.Region,
			}
			return true, newRegion, errors.Annotatef(berrors.ErrKVNotLeader, "not leader: %s", errPb.GetMessage())
		}
	case errPb.EpochNotMatch != nil:
		if currentRegions := errPb.GetEpochNotMatch().GetCurrentRegions(); currentRegions != nil {
			var currentRegion *metapb.Region
			for _, r := range currentRegions {
				if insideRegion(r, meta) {
					currentRegion = r
					break
				}
			}
			if currentRegion != nil {
				var newLeader *metapb.Peer
				for _, p := range currentRegion.Peers {
					if p.GetStoreId() == region.Leader.GetStoreId() {
						newLeader = p
						break
					}
				}
				if newLeader != nil {
					newRegion = &RegionInfo{
						Leader: newLeader,
						Region: currentRegion,
					}
				}
			}
		}
		return true, newRegion, errors.Annotatef(berrors.ErrKVEpochNotMatch, "epoch not match: %s", errPb.GetMessage())
	}
	return false, nil, errors.Annotatef(berrors.ErrKVUnknown, "non retryable error: %s", resp.GetError().GetMessage())
}

func insideRegion(region *metapb.Region, meta *sst.SSTMeta) bool {
	rg := meta.GetRange()
	return keyInsideRegion(region, rg.GetStart()) && keyInsideRegion(region, rg.GetEnd())
}
