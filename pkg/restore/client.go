package restore

import (
	"context"
	"fmt"
	"strings"

	restore_util "github.com/5kbpers/tidb-tools/pkg/restore-util"
	"github.com/pingcap/br/pkg/meta"
	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/backup"
	"github.com/pingcap/log"
	"github.com/pingcap/parser/model"
	pd "github.com/pingcap/pd/client"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/session"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/store/tikv"
	"go.uber.org/zap"
)

// Client sends requests to importer to restore files
type Client struct {
	ctx    context.Context
	cancel context.CancelFunc

	pdClient     pd.Client
	pdAddrs      []string
	tikvCli      tikv.Storage
	fileImporter FileImporter

	databases  map[string]*Database
	dbDSN      string
	backupMeta *backup.BackupMeta
}

// NewRestoreClient returns a new RestoreClient
func NewRestoreClient(ctx context.Context, pdAddrs string) (*Client, error) {
	_ctx, cancel := context.WithCancel(ctx)
	addrs := strings.Split(pdAddrs, ",")
	pdClient, err := pd.NewClient(addrs, pd.SecurityOption{})
	if err != nil {
		return nil, errors.Trace(err)
	}
	log.Info("new region client", zap.String("pdAddrs", pdAddrs))
	tikvCli, err := tikv.Driver{}.Open(
		// Disable GC because TiDB enables GC already.
		fmt.Sprintf("tikv://%s?disableGC=true", pdAddrs))
	if err != nil {
		return nil, errors.Trace(err)
	}

	return &Client{
		ctx:      _ctx,
		cancel:   cancel,
		pdClient: pdClient,
		pdAddrs:  addrs,
		tikvCli:  tikvCli.(tikv.Storage),
	}, nil
}

// InitBackupMeta loads schemas from BackupMeta to initialize RestoreClient
func (rc *Client) InitBackupMeta(backupMeta *backup.BackupMeta) error {
	databases, err := LoadBackupTables(backupMeta)
	if err != nil {
		return errors.Trace(err)
	}
	rc.databases = databases
	rc.backupMeta = backupMeta

	client, err := restore_util.NewClient(rc.pdClient)
	if err != nil {
		return err
	}
	rc.fileImporter = NewFileImporter(rc.ctx, client, backupMeta.GetPath())
	return nil
}

// SetDbDSN sets the DNS to connect the database to a new value
func (rc *Client) SetDbDSN(dns string) {
	rc.dbDSN = dns
}

// GetDbDSN returns a DNS to connect the database
func (rc *Client) GetDbDSN() string {
	return rc.dbDSN
}

// GetTS gets a new timestamp from PD
func (rc *Client) GetTS() (uint64, error) {
	p, l, err := rc.pdClient.GetTS(rc.ctx)
	if err != nil {
		return 0, errors.Trace(err)
	}
	ts := meta.Timestamp{
		Physical: p,
		Logical:  l,
	}
	restoreTS := meta.EncodeTs(ts)
	log.Info("restore timestamp", zap.Uint64("RestoreTS", restoreTS))
	return restoreTS, nil
}

// GetDatabase returns a database by name
func (rc *Client) GetDatabase(name string) *Database {
	return rc.databases[name]
}

func (rc *Client) GetTableSchema(dbName model.CIStr, tableName model.CIStr, restoreTS uint64) (*model.TableInfo, error) {
	dbSession, err := session.CreateSession(rc.tikvCli)
	if err != nil {
		return nil, errors.Trace(err)
	}
	do := domain.GetDomain(dbSession.(sessionctx.Context))
	info, err := do.GetSnapshotInfoSchema(restoreTS)
	if err != nil {
		return nil, errors.Trace(err)
	}
	table, err := info.TableByName(dbName, tableName)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return table.Meta(), nil
}

func (rc *Client) RestoreTable(table *Table, restoreTS uint64) error {
	log.Info("start to restore table",
		zap.Stringer("table", table.Schema.Name),
		zap.Stringer("db", table.Db.Name),
	)
	err := CreateTable(table, rc.dbDSN)
	if err != nil {
		return errors.Trace(err)
	}
	err = AlterAutoIncID(table, rc.dbDSN)
	if err != nil {
		return errors.Trace(err)
	}
	newTableInfo, returnErr := rc.GetTableSchema(table.Db.Name, table.Schema.Name, restoreTS)
	if returnErr != nil {
		return errors.Trace(returnErr)
	}
	recordRule, indexRules := GetRewriteRules(table.Schema, newTableInfo)
	errCh := make(chan error, len(table.Files))
	defer close(errCh)
	for _, file := range table.Files {
		go func(file *File) {
			select {
			case <-rc.ctx.Done():
			case errCh <- rc.fileImporter.Import(file, recordRule, indexRules):
			}
		}(file)
	}
	for range table.Files {
		err = <-errCh
		if err != nil {
			rc.cancel()
			return err
		}
	}
	return nil
}

// RestoreDatabase executes the job to restore a database
func (rc *Client) RestoreDatabase(db *Database, restoreTS uint64) error {
	err := CreateDatabase(db.Schema, rc.dbDSN)
	if err != nil {
		return err
	}
	errCh := make(chan error, len(db.Tables))
	defer close(errCh)
	for _, table := range db.Tables {
		go func(table *Table) {
			select {
			case <-rc.ctx.Done():
			case errCh <- rc.RestoreTable(table, restoreTS):
			}
		}(table)
	}
	for range db.Tables {
		err = <-errCh
		if err != nil {
			return err
		}
	}
	return nil
}

// RestoreAll executes the job to restore all files
func (rc *Client) RestoreAll(restoreTS uint64) error {
	errCh := make(chan error, len(rc.databases))
	defer close(errCh)
	for _, db := range rc.databases {
		go func(db *Database) {
			select {
			case <-rc.ctx.Done():
			case errCh <- rc.RestoreDatabase(db, restoreTS):
			}
		}(db)
	}
	for range rc.databases {
		err := <-errCh
		if err != nil {
			return err
		}
	}
	return nil
}
