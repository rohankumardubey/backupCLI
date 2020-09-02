// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package cdclog

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"

	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	timodel "github.com/pingcap/parser/model"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/tidb/types"
	"go.uber.org/zap"
)

// ColumnFlagType represents the type of column.
type ColumnFlagType uint64

// ItemType represents the type of SortItem.
type ItemType uint

const (
	// RowChanged represents dml type.
	RowChanged ItemType = 1 << ItemType(iota)
	// DDL represents ddl type.
	DDL
)

const (
	// BatchVersion1 represents the version of batch format.
	BatchVersion1 uint64 = 1
	// BinaryFlag means the column charset is binary.
	BinaryFlag ColumnFlagType = 1 << ColumnFlagType(iota)
	// HandleKeyFlag means the column is selected as the handle key.
	HandleKeyFlag
	// GeneratedColumnFlag means the column is a generated column.
	GeneratedColumnFlag
	// PrimaryKeyFlag means the column is primary key.
	PrimaryKeyFlag
	// UniqueKeyFlag means the column is unique key.
	UniqueKeyFlag
	// MultipleKeyFlag means the column is multiple key.
	MultipleKeyFlag
	// NullableFlag means the column is nullable.
	NullableFlag
)

type column struct {
	Type byte `json:"t"`

	// WhereHandle is deprecated
	// WhereHandle is replaced by HandleKey in Flag.
	WhereHandle *bool          `json:"h,omitempty"`
	Flag        ColumnFlagType `json:"f"`
	Value       interface{}    `json:"v"`
}

func (c column) toDatum() (types.Datum, error) {
	var (
		val interface{}
		err error
	)

	switch c.Type {
	case types.KindInt64, types.KindUint64:
		val, err = c.Value.(json.Number).Int64()
		if err != nil {
			return types.Datum{}, errors.Trace(err)
		}
	case types.KindFloat32, types.KindFloat64:
		val, err = c.Value.(json.Number).Float64()
		if err != nil {
			return types.Datum{}, errors.Trace(err)
		}
	default:
		val = c.Value
	}
	return types.NewDatum(val), nil
}

func formatColumnVal(c column) column {
	switch c.Type {
	case mysql.TypeTinyBlob, mysql.TypeMediumBlob,
		mysql.TypeLongBlob, mysql.TypeBlob:
		if s, ok := c.Value.(string); ok {
			var err error
			c.Value, err = base64.StdEncoding.DecodeString(s)
			if err != nil {
				log.Fatal("invalid column value, please report a bug", zap.Any("col", c), zap.Error(err))
			}
		}
	case mysql.TypeBit:
		if s, ok := c.Value.(json.Number); ok {
			intNum, err := s.Int64()
			if err != nil {
				log.Fatal("invalid column value, please report a bug", zap.Any("col", c), zap.Error(err))
			}
			c.Value = uint64(intNum)
		}
	}
	return c
}

type messageKey struct {
	Ts        uint64 `json:"ts"`
	Schema    string `json:"scm,omitempty"`
	Table     string `json:"tbl,omitempty"`
	RowID     int64  `json:"rid,omitempty"`
	Partition *int64 `json:"ptn,omitempty"`
}

// Encode the messageKey.
func (m *messageKey) Encode() ([]byte, error) {
	return json.Marshal(m)
}

// Decode the messageKey.
func (m *messageKey) Decode(data []byte) error {
	return json.Unmarshal(data, m)
}

// MessageDDL represents the ddl changes.
type MessageDDL struct {
	Query string             `json:"q"`
	Type  timodel.ActionType `json:"t"`
}

// Encode the DDL message.
func (m *MessageDDL) Encode() ([]byte, error) {
	return json.Marshal(m)
}

// Decode the DDL message.
func (m *MessageDDL) Decode(data []byte) error {
	return json.Unmarshal(data, m)
}

// MessageRow represents the row changes in same commit ts.
type MessageRow struct {
	Update     map[string]column `json:"u,omitempty"`
	PreColumns map[string]column `json:"p,omitempty"`
	Delete     map[string]column `json:"d,omitempty"`
}

// Encode the Row message.
func (m *MessageRow) Encode() ([]byte, error) {
	return json.Marshal(m)
}

// Decode the Row message.
func (m *MessageRow) Decode(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	err := decoder.Decode(m)
	if err != nil {
		return errors.Trace(err)
	}
	for colName, column := range m.Update {
		m.Update[colName] = formatColumnVal(column)
	}
	for colName, column := range m.Delete {
		m.Delete[colName] = formatColumnVal(column)
	}
	for colName, column := range m.PreColumns {
		m.PreColumns[colName] = formatColumnVal(column)
	}
	return nil
}

// SortItem represents a DDL item or Row changed item.
type SortItem struct {
	ItemType ItemType
	Meta     interface{}
	Schema   string
	Table    string
	RowID    int64
	TS       uint64
}

// LessThan return whether it has smaller commit ts than other item.
func (s *SortItem) LessThan(other *SortItem) bool {
	if other != nil {
		return s.TS < other.TS
	}
	return false
}

// JSONEventBatchMixedDecoder decodes the byte of a batch into the original messages.
type JSONEventBatchMixedDecoder struct {
	mixedBytes []byte
}

func (b *JSONEventBatchMixedDecoder) decodeNextKey() (*messageKey, uint64, error) {
	keyLen := binary.BigEndian.Uint64(b.mixedBytes[:8])
	key := b.mixedBytes[8 : keyLen+8]
	// drop value bytes
	msgKey := new(messageKey)
	err := msgKey.Decode(key)
	if err != nil {
		return nil, 0, errors.Trace(err)
	}
	return msgKey, keyLen, nil
}

// NextEvent return next item depends on type
func (b *JSONEventBatchMixedDecoder) NextEvent(itemType ItemType) (*SortItem, error) {
	if !b.HasNext() {
		return nil, nil
	}
	nextKey, nextKeyLen, err := b.decodeNextKey()
	if err != nil {
		return nil, err
	}

	b.mixedBytes = b.mixedBytes[nextKeyLen+8:]
	valueLen := binary.BigEndian.Uint64(b.mixedBytes[:8])
	value := b.mixedBytes[8 : valueLen+8]
	b.mixedBytes = b.mixedBytes[valueLen+8:]

	var m interface{}
	if itemType == DDL {
		m = new(MessageDDL)
		if err := m.(*MessageDDL).Decode(value); err != nil {
			return nil, errors.Trace(err)
		}
	} else if itemType == RowChanged {
		m = new(MessageRow)
		if err := m.(*MessageRow).Decode(value); err != nil {
			return nil, errors.Trace(err)
		}
	}

	item := &SortItem{
		ItemType: itemType,
		Meta:     m,
		Schema:   nextKey.Schema,
		Table:    nextKey.Table,
		TS:       nextKey.Ts,
		RowID:    nextKey.RowID,
	}
	return item, nil
}

// HasNext represents whether it has next kv to decode.
func (b *JSONEventBatchMixedDecoder) HasNext() bool {
	return len(b.mixedBytes) > 0
}

// NewJSONEventBatchDecoder creates a new JSONEventBatchDecoder.
func NewJSONEventBatchDecoder(data []byte) (*JSONEventBatchMixedDecoder, error) {
	version := binary.BigEndian.Uint64(data[:8])
	data = data[8:]
	if version != BatchVersion1 {
		return nil, errors.New("unexpected key format version")
	}
	return &JSONEventBatchMixedDecoder{
		mixedBytes: data,
	}, nil
}
