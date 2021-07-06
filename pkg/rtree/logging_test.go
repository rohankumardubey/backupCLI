// Copyright 2021 PingCAP, Inc. Licensed under Apache-2.0.

package rtree_test

import (
	"fmt"
	"strings"

	. "github.com/pingcap/check"
	backuppb "github.com/pingcap/kvproto/pkg/backup"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/pingcap/br/pkg/rtree"
)

var _ = Suite(&testLoggingSuite{})

type testLoggingSuite struct{}

func (s *testLoggingSuite) TestLogRanges(c *C) {
	cases := []struct {
		count  int
		expect string
	}{
		{0, `{"ranges": {"total": 0, "ranges": [], "totalFiles": 0, "totalKVs": 0, "totalBytes": 0, "totalSize": 0}}`},
		{1, `{"ranges": {"total": 1, "ranges": ["[30, 31)"], "totalFiles": 1, "totalKVs": 0, "totalBytes": 0, "totalSize": 0}}`},
		{2, `{"ranges": {"total": 2, "ranges": ["[30, 31)", "[31, 32)"], "totalFiles": 2, "totalKVs": 1, "totalBytes": 1, "totalSize": 1}}`},
		{3, `{"ranges": {"total": 3, "ranges": ["[30, 31)", "[31, 32)", "[32, 33)"], "totalFiles": 3, "totalKVs": 3, "totalBytes": 3, "totalSize": 3}}`},
		{4, `{"ranges": {"total": 4, "ranges": ["[30, 31)", "[31, 32)", "[32, 33)", "[33, 34)"], "totalFiles": 4, "totalKVs": 6, "totalBytes": 6, "totalSize": 6}}`},
		{5, `{"ranges": {"total": 5, "ranges": ["[30, 31)", "(skip 3)", "[34, 35)"], "totalFiles": 5, "totalKVs": 10, "totalBytes": 10, "totalSize": 10}}`},
		{6, `{"ranges": {"total": 6, "ranges": ["[30, 31)", "(skip 4)", "[35, 36)"], "totalFiles": 6, "totalKVs": 15, "totalBytes": 15, "totalSize": 15}}`},
		{1024, `{"ranges": {"total": 1024, "ranges": ["[30, 31)", "(skip 1022)", "[31303233, 31303234)"], "totalFiles": 1024, "totalKVs": 523776, "totalBytes": 523776, "totalSize": 523776}}`},
	}

	encoder := zapcore.NewConsoleEncoder(zapcore.EncoderConfig{})
	for _, cs := range cases {
		ranges := make([]rtree.Range, cs.count)
		for j := 0; j < cs.count; j++ {
			ranges[j] = *newRange([]byte(fmt.Sprintf("%d", j)), []byte(fmt.Sprintf("%d", j+1)))
			ranges[j].Files = append(ranges[j].Files, &backuppb.File{TotalKvs: uint64(j), TotalBytes: uint64(j)})
		}
		out, err := encoder.EncodeEntry(zapcore.Entry{}, []zap.Field{rtree.ZapRanges(ranges)})
		c.Assert(err, IsNil)
		c.Assert(strings.TrimRight(out.String(), "\n"), Equals, cs.expect)
	}
}
