// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package storage

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	. "github.com/pingcap/check"
)

func (r *testStorageSuite) TestWithCompressReadWriteFile(c *C) {
	dir := c.MkDir()
<<<<<<< HEAD
	backend, err := ParseBackend("local://"+filepath.ToSlash(dir), nil)
=======
	backend, err := ParseBackend("local:///"+dir, nil)
>>>>>>> bd3f4577 (storage/: refactor storage.ExternalStorage interface (#676))
	c.Assert(err, IsNil)
	ctx := context.Background()
	storage, err := Create(ctx, backend, true)
	c.Assert(err, IsNil)
	storage = WithCompression(storage, Gzip)
	name := "with compress test"
	content := "hello,world!"
	fileName := strings.ReplaceAll(name, " ", "-") + ".txt.gz"
	err = storage.WriteFile(ctx, fileName, []byte(content))
	c.Assert(err, IsNil)

	// make sure compressed file is written correctly
	file, err := os.Open(filepath.Join(dir, fileName))
	c.Assert(err, IsNil)
	uncompressedFile, err := newCompressReader(Gzip, file)
	c.Assert(err, IsNil)
	newContent, err := ioutil.ReadAll(uncompressedFile)
	c.Assert(err, IsNil)
	c.Assert(string(newContent), Equals, content)

	// test withCompression ReadFile
	newContent, err = storage.ReadFile(ctx, fileName)
	c.Assert(err, IsNil)
	c.Assert(string(newContent), Equals, content)
}
