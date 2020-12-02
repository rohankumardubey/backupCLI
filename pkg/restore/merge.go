// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package restore

import (
	"strings"

	"github.com/pingcap/errors"
	kvproto "github.com/pingcap/kvproto/pkg/backup"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/tablecodec"

	berrors "github.com/pingcap/br/pkg/errors"
	"github.com/pingcap/br/pkg/rtree"
	"github.com/pingcap/br/pkg/utils"
)

const (
	// DefaultMergeRegionSizeBytes is the default region split size, 96MB.
	// See https://github.com/tikv/tikv/blob/v4.0.8/components/raftstore/src/coprocessor/config.rs#L35-L38
	DefaultMergeRegionSizeBytes uint64 = 96 * utils.MB

	// DefaultMergeRegionKeyCount is the default region key count, 960000.
	DefaultMergeRegionKeyCount uint64 = 960000

	writeCFName   = "write"
	defaultCFName = "default"
)

// MergeRangesStat holds statistics for the MergeRanges.
type MergeRangesStat struct {
	TotalFiles           int
	TotalWriteCFFile     int
	TotalDefaultCFFile   int
	TotalRegions         int
	RegionKeysAvg        int
	RegionBytesAvg       int
	MergedRegions        int
	MergedRegionKeysAvg  int
	MergedRegionBytesAvg int
}

// MergeFileRanges returns ranges of the files are merged based on
// splitSizeBytes and splitKeyCount.
//
// By merging small ranges, it speeds up restoring a backup that contains many
// small ranges (regions) as it reduces split region and scatter region.
func MergeFileRanges(
	files []*kvproto.File, splitSizeBytes, splitKeyCount uint64,
) ([]rtree.Range, *MergeRangesStat, error) {
	if len(files) == 0 {
		return []rtree.Range{}, &MergeRangesStat{}, nil
	}
	totalBytes := uint64(0)
	totalKvs := uint64(0)
	totalFiles := len(files)
	writeCFFile := 0
	defaultCFFile := 0

	filesMap := make(map[string][]*kvproto.File)
	for _, file := range files {
		filesMap[string(file.StartKey)] = append(filesMap[string(file.StartKey)], file)

		// We skips all default cf files because we don't range overlap.
		if file.Cf == writeCFName || strings.Contains(file.GetName(), writeCFName) {
			writeCFFile++
		} else {
			defaultCFFile++
		}
		totalBytes += file.TotalBytes
		totalKvs += file.TotalKvs
	}

	// Check if files are overlapped
	rangeTree := rtree.NewRangeTree()
	for key := range filesMap {
		files := filesMap[key]
		if out := rangeTree.InsertRange(rtree.Range{
			StartKey: files[0].GetStartKey(),
			EndKey:   files[0].GetEndKey(),
			Files:    files,
		}); out != nil {
			return nil, nil, errors.Annotatef(berrors.ErrRestoreInvalidRange,
				"duplicate range %s files %+v", out, files)
		}
	}

	needMerge := func(left, right *rtree.Range) bool {
		leftBytes, leftKeys := left.BytesAndKeys()
		rightBytes, rightKeys := right.BytesAndKeys()
		if rightBytes == 0 {
			return true
		}
		if leftBytes+rightBytes > splitSizeBytes {
			return false
		}
		if leftKeys+rightKeys > splitKeyCount {
			return false
		}
		// Do not merge ranges in different tables.
		if tablecodec.DecodeTableID(kv.Key(left.StartKey)) != tablecodec.DecodeTableID(kv.Key(right.StartKey)) {
			return false
		}
		return true
	}
	sortedRanges := rangeTree.GetSortedRanges()
	for i := 1; i < len(sortedRanges); {
		if !needMerge(&sortedRanges[i-1], &sortedRanges[i]) {
			i++
			continue
		}
		sortedRanges[i-1].EndKey = sortedRanges[i].EndKey
		sortedRanges[i-1].Files = append(sortedRanges[i-1].Files, sortedRanges[i].Files...)
		// TODO: this is slow when there are lots of ranges need to merge.
		sortedRanges = append(sortedRanges[:i], sortedRanges[i+1:]...)
	}

	regionBytesAvg := totalBytes / uint64(writeCFFile)
	regionKeysAvg := totalKvs / uint64(writeCFFile)
	mergedRegionBytesAvg := totalBytes / uint64(len(sortedRanges))
	mergedRegionKeysAvg := totalKvs / uint64(len(sortedRanges))

	return sortedRanges, &MergeRangesStat{
		TotalFiles:           totalFiles,
		TotalWriteCFFile:     writeCFFile,
		TotalDefaultCFFile:   defaultCFFile,
		TotalRegions:         writeCFFile,
		RegionKeysAvg:        int(regionKeysAvg),
		RegionBytesAvg:       int(regionBytesAvg),
		MergedRegions:        len(sortedRanges),
		MergedRegionKeysAvg:  int(mergedRegionKeysAvg),
		MergedRegionBytesAvg: int(mergedRegionBytesAvg),
	}, nil
}
