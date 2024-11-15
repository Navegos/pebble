// Copyright 2013 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/manifest"
	"github.com/cockroachdb/pebble/internal/testkeys"
	"github.com/cockroachdb/pebble/objstorage"
	"github.com/cockroachdb/pebble/objstorage/objstorageprovider"
	"github.com/cockroachdb/pebble/sstable"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/stretchr/testify/require"
)

type fileCacheTestFile struct {
	vfs.File
	fs   *fileCacheTestFS
	name string
}

func (f *fileCacheTestFile) Close() error {
	f.fs.mu.Lock()
	if f.fs.closeCounts != nil {
		f.fs.closeCounts[f.name]++
	}
	f.fs.mu.Unlock()
	return f.File.Close()
}

type fileCacheTestFS struct {
	vfs.FS

	mu               sync.Mutex
	openCounts       map[string]int
	closeCounts      map[string]int
	openErrorEnabled bool
}

func (fs *fileCacheTestFS) Open(name string, opts ...vfs.OpenOption) (vfs.File, error) {
	fs.mu.Lock()
	if fs.openErrorEnabled {
		fs.mu.Unlock()
		return nil, errors.New("injected error")
	}
	if fs.openCounts != nil {
		fs.openCounts[name]++
	}
	fs.mu.Unlock()
	f, err := fs.FS.Open(name, opts...)
	if len(opts) < 1 || opts[0] != vfs.RandomReadsOption {
		return nil, errors.Errorf("sstable file %s not opened with random reads option", name)
	}
	if err != nil {
		return nil, err
	}
	return &fileCacheTestFile{f, fs, name}, nil
}

func (fs *fileCacheTestFS) validate(
	t *testing.T, c *fileCacheContainer, f func(i, gotO, gotC int) error,
) {
	if err := fs.validateOpenTables(f); err != nil {
		t.Error(err)
		return
	}
	c.close()
	if err := fs.validateNoneStillOpen(); err != nil {
		t.Error(err)
		return
	}
}

func (fs *fileCacheTestFS) setOpenError(enabled bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.openErrorEnabled = enabled
}

// validateOpenTables validates that no tables in the cache are open twice, and
// the number still open is no greater than fileCacheTestCacheSize.
func (fs *fileCacheTestFS) validateOpenTables(f func(i, gotO, gotC int) error) error {
	// try backs off to let any clean-up goroutines do their work.
	return try(100*time.Microsecond, 20*time.Second, func() error {
		fs.mu.Lock()
		defer fs.mu.Unlock()

		numStillOpen := 0
		for i := 0; i < fileCacheTestNumTables; i++ {
			filename := base.MakeFilepath(fs, "", fileTypeTable, base.DiskFileNum(i))
			gotO, gotC := fs.openCounts[filename], fs.closeCounts[filename]
			if gotO > gotC {
				numStillOpen++
			}
			if gotC != gotO && gotC != gotO-1 {
				return errors.Errorf("i=%d: table closed too many or too few times: opened %d times, closed %d times",
					i, gotO, gotC)
			}
			if f != nil {
				if err := f(i, gotO, gotC); err != nil {
					return err
				}
			}
		}
		if numStillOpen > fileCacheTestCacheSize {
			return errors.Errorf("numStillOpen is %d, want <= %d", numStillOpen, fileCacheTestCacheSize)
		}
		return nil
	})
}

// validateNoneStillOpen validates that no tables in the cache are open.
func (fs *fileCacheTestFS) validateNoneStillOpen() error {
	// try backs off to let any clean-up goroutines do their work.
	return try(100*time.Microsecond, 20*time.Second, func() error {
		fs.mu.Lock()
		defer fs.mu.Unlock()

		for i := 0; i < fileCacheTestNumTables; i++ {
			filename := base.MakeFilepath(fs, "", fileTypeTable, base.DiskFileNum(i))
			gotO, gotC := fs.openCounts[filename], fs.closeCounts[filename]
			if gotO != gotC {
				return errors.Errorf("i=%d: opened %d times, closed %d times", i, gotO, gotC)
			}
		}
		return nil
	})
}

const (
	fileCacheTestNumTables = 300
	fileCacheTestCacheSize = 100
)

// newFileCacheTest returns a shareable file cache to be used for tests.
// It is the caller's responsibility to unref the file cache.
func newFileCacheTest(size int64, fileCacheSize int, numShards int) *FileCache {
	cache := NewCache(size)
	defer cache.Unref()
	return NewFileCache(cache, numShards, fileCacheSize)
}

func newFileCacheContainerTest(
	tc *FileCache, dirname string,
) (*fileCacheContainer, *fileCacheTestFS, error) {
	xxx := bytes.Repeat([]byte("x"), fileCacheTestNumTables)
	fs := &fileCacheTestFS{
		FS: vfs.NewMem(),
	}
	objProvider, err := objstorageprovider.Open(objstorageprovider.DefaultSettings(fs, dirname))
	if err != nil {
		return nil, nil, err
	}
	defer objProvider.Close()

	for i := 0; i < fileCacheTestNumTables; i++ {
		w, _, err := objProvider.Create(context.Background(), fileTypeTable, base.DiskFileNum(i), objstorage.CreateOptions{})
		if err != nil {
			return nil, nil, errors.Wrap(err, "fs.Create")
		}
		tw := sstable.NewWriter(w, sstable.WriterOptions{TableFormat: sstable.TableFormatPebblev2})
		ik := base.ParseInternalKey(fmt.Sprintf("k.SET.%d", i))
		if err := tw.Raw().AddWithForceObsolete(ik, xxx[:i], false); err != nil {
			return nil, nil, errors.Wrap(err, "tw.Set")
		}
		if err := tw.RangeKeySet([]byte("k"), []byte("l"), nil, xxx[:i]); err != nil {
			return nil, nil, errors.Wrap(err, "tw.Set")
		}
		if err := tw.Close(); err != nil {
			return nil, nil, errors.Wrap(err, "tw.Close")
		}
	}

	fs.mu.Lock()
	fs.openCounts = map[string]int{}
	fs.closeCounts = map[string]int{}
	fs.mu.Unlock()

	opts := &Options{}
	opts.EnsureDefaults()
	if tc == nil {
		opts.Cache = NewCache(8 << 20) // 8 MB
		defer opts.Cache.Unref()
	} else {
		opts.Cache = tc.cache
	}

	c := newFileCacheContainer(tc, opts.Cache.NewID(), objProvider, opts, fileCacheTestCacheSize,
		&sstable.CategoryStatsCollector{})
	return c, fs, nil
}

// Test basic reference counting for the file cache.
func TestFileCacheRefs(t *testing.T) {
	c := newFileCacheTest(8<<20, 10, 2)

	v := c.refs.Load()
	if v != 1 {
		require.Equal(t, 1, v)
	}

	c.Ref()
	v = c.refs.Load()
	if v != 2 {
		require.Equal(t, 2, v)
	}

	c.Unref()
	v = c.refs.Load()
	if v != 1 {
		require.Equal(t, 1, v)
	}

	c.Unref()
	v = c.refs.Load()
	if v != 0 {
		require.Equal(t, 0, v)
	}

	defer func() {
		if r := recover(); r != nil {
			if fmt.Sprint(r) != "pebble: inconsistent reference count: -1" {
				t.Fatalf("unexpected panic message")
			}
		} else if r == nil {
			t.Fatalf("expected panic")
		}
	}()
	c.Unref()
}

// Basic test to determine if reads through the file cache are wired correctly.
func TestVirtualReadsWiring(t *testing.T) {
	var d *DB
	var err error
	d, err = Open("",
		&Options{
			FS:                 vfs.NewMem(),
			FormatMajorVersion: internalFormatNewest,
			Comparer:           testkeys.Comparer,
			// Compactions which conflict with virtual sstable creation can be
			// picked by Pebble. We disable that.
			DisableAutomaticCompactions: true,
		})
	require.NoError(t, err)

	b := newBatch(d)
	// Some combination of sets, range deletes, and range key sets/unsets, so
	// all of the file cache iterator functions are utilized.
	require.NoError(t, b.Set([]byte{'a'}, []byte{'a'}, nil))                           // SeqNum start.
	require.NoError(t, b.Set([]byte{'d'}, []byte{'d'}, nil))                           // SeqNum: +1
	require.NoError(t, b.DeleteRange([]byte{'c'}, []byte{'e'}, nil))                   // SeqNum: +2
	require.NoError(t, b.Set([]byte{'f'}, []byte{'f'}, nil))                           // SeqNum: +3
	require.NoError(t, b.RangeKeySet([]byte{'f'}, []byte{'k'}, nil, []byte{'c'}, nil)) // SeqNum: +4
	require.NoError(t, b.RangeKeyUnset([]byte{'j'}, []byte{'k'}, nil, nil))            // SeqNum: +5
	require.NoError(t, b.Set([]byte{'z'}, []byte{'z'}, nil))                           // SeqNum: +6
	require.NoError(t, d.Apply(b, nil))
	require.NoError(t, d.Flush())
	require.NoError(t, d.Compact([]byte{'a'}, []byte{'b'}, false))
	require.Equal(t, 1, int(d.Metrics().Levels[6].NumFiles))

	d.mu.Lock()

	// Virtualize the single sstable in the lsm.

	currVersion := d.mu.versions.currentVersion()
	l6 := currVersion.Levels[6]
	l6FileIter := l6.Iter()
	parentFile := l6FileIter.First()
	f1 := FileNum(d.mu.versions.nextFileNum.Load())
	f2 := f1 + 1
	d.mu.versions.nextFileNum.Add(2)

	seqNumA := parentFile.Smallest.SeqNum()
	// See SeqNum comments above.
	seqNumCEDel := seqNumA + 2
	seqNumRangeSet := seqNumA + 4
	seqNumRangeUnset := seqNumA + 5
	seqNumZ := seqNumA + 6

	v1 := &manifest.FileMetadata{
		FileBacking:           parentFile.FileBacking,
		FileNum:               f1,
		CreationTime:          time.Now().Unix(),
		Size:                  parentFile.Size / 2,
		SmallestSeqNum:        parentFile.SmallestSeqNum,
		LargestSeqNum:         parentFile.LargestSeqNum,
		LargestSeqNumAbsolute: parentFile.LargestSeqNumAbsolute,
		Smallest:              base.MakeInternalKey([]byte{'a'}, seqNumA, InternalKeyKindSet),
		Largest:               base.MakeInternalKey([]byte{'a'}, seqNumA, InternalKeyKindSet),
		SmallestPointKey:      base.MakeInternalKey([]byte{'a'}, seqNumA, InternalKeyKindSet),
		LargestPointKey:       base.MakeInternalKey([]byte{'a'}, seqNumA, InternalKeyKindSet),
		HasPointKeys:          true,
		Virtual:               true,
	}
	v1.Stats.NumEntries = 1

	v2 := &manifest.FileMetadata{
		FileBacking:           parentFile.FileBacking,
		FileNum:               f2,
		CreationTime:          time.Now().Unix(),
		Size:                  parentFile.Size / 2,
		SmallestSeqNum:        parentFile.SmallestSeqNum,
		LargestSeqNum:         parentFile.LargestSeqNum,
		LargestSeqNumAbsolute: parentFile.LargestSeqNumAbsolute,
		Smallest:              base.MakeInternalKey([]byte{'d'}, seqNumCEDel, InternalKeyKindRangeDelete),
		Largest:               base.MakeInternalKey([]byte{'z'}, seqNumZ, InternalKeyKindSet),
		SmallestPointKey:      base.MakeInternalKey([]byte{'d'}, seqNumCEDel, InternalKeyKindRangeDelete),
		LargestPointKey:       base.MakeInternalKey([]byte{'z'}, seqNumZ, InternalKeyKindSet),
		SmallestRangeKey:      base.MakeInternalKey([]byte{'f'}, seqNumRangeSet, InternalKeyKindRangeKeySet),
		LargestRangeKey:       base.MakeInternalKey([]byte{'k'}, seqNumRangeUnset, InternalKeyKindRangeKeyUnset),
		HasPointKeys:          true,
		Virtual:               true,
	}
	v2.Stats.NumEntries = 6

	v1.LargestPointKey = v1.Largest
	v1.SmallestPointKey = v1.Smallest

	v2.LargestPointKey = v2.Largest
	v2.SmallestPointKey = v2.Smallest

	v1.ValidateVirtual(parentFile)
	d.checkVirtualBounds(v1)
	v2.ValidateVirtual(parentFile)
	d.checkVirtualBounds(v2)

	// Write the version edit.
	fileMetrics := func(ve *versionEdit) map[int]*LevelMetrics {
		metrics := newFileMetrics(ve.NewFiles)
		for de, f := range ve.DeletedFiles {
			lm := metrics[de.Level]
			if lm == nil {
				lm = &LevelMetrics{}
				metrics[de.Level] = lm
			}
			metrics[de.Level].NumFiles--
			metrics[de.Level].Size -= int64(f.Size)
		}
		return metrics
	}

	applyVE := func(ve *versionEdit) error {
		d.mu.versions.logLock()
		jobID := d.newJobIDLocked()

		err := d.mu.versions.logAndApply(jobID, ve, fileMetrics(ve), false, func() []compactionInfo {
			return d.getInProgressCompactionInfoLocked(nil)
		})
		d.updateReadStateLocked(nil)
		return err
	}

	ve := manifest.VersionEdit{}
	d1 := manifest.DeletedFileEntry{Level: 6, FileNum: parentFile.FileNum}
	n1 := manifest.NewFileEntry{Level: 6, Meta: v1}
	n2 := manifest.NewFileEntry{Level: 6, Meta: v2}

	ve.DeletedFiles = make(map[manifest.DeletedFileEntry]*manifest.FileMetadata)
	ve.DeletedFiles[d1] = parentFile
	ve.NewFiles = append(ve.NewFiles, n1)
	ve.NewFiles = append(ve.NewFiles, n2)
	ve.CreatedBackingTables = append(ve.CreatedBackingTables, parentFile.FileBacking)

	require.NoError(t, applyVE(&ve))

	currVersion = d.mu.versions.currentVersion()
	l6 = currVersion.Levels[6]
	l6FileIter = l6.Iter()
	for f := l6FileIter.First(); f != nil; f = l6FileIter.Next() {
		require.Equal(t, true, f.Virtual)
	}
	d.mu.Unlock()

	// Confirm that there were only 2 virtual sstables in L6.
	require.Equal(t, 2, int(d.Metrics().Levels[6].NumFiles))

	// These reads will go through the file cache.
	iter, _ := d.NewIter(nil)
	expected := []byte{'a', 'f', 'z'}
	for i, x := 0, iter.First(); x; i, x = i+1, iter.Next() {
		require.Equal(t, []byte{expected[i]}, iter.Value())
	}
	iter.Close()

	// We don't defer this Close in case we get a panic while holding d.mu.
	d.Close()
}

// The file cache shouldn't be usable after all the dbs close.
func TestSharedFileCacheUseAfterAllFree(t *testing.T) {
	fc := newFileCacheTest(8<<20, 10, 1)
	db1, err := Open("test",
		&Options{
			FS:        vfs.NewMem(),
			Cache:     fc.cache,
			FileCache: fc,
		})
	require.NoError(t, err)

	// Release our reference, now that the db has a reference.
	fc.Unref()

	db2, err := Open("test",
		&Options{
			FS:        vfs.NewMem(),
			Cache:     fc.cache,
			FileCache: fc,
		})
	require.NoError(t, err)

	require.NoError(t, db1.Close())
	require.NoError(t, db2.Close())

	v := fc.refs.Load()
	if v != 0 {
		t.Fatalf("expected reference count %d, got %d", 0, v)
	}

	defer func() {
		// The cache ref gets incremented before the panic, so we should
		// decrement it to prevent the finalizer from detecting a leak.
		fc.cache.Unref()

		if r := recover(); r != nil {
			if fmt.Sprint(r) != "pebble: inconsistent reference count: 1" {
				t.Fatalf("unexpected panic message")
			}
		} else if r == nil {
			t.Fatalf("expected panic")
		}
	}()

	db3, _ := Open("test",
		&Options{
			FS:        vfs.NewMem(),
			Cache:     fc.cache,
			FileCache: fc,
		})
	_ = db3
}

// TestSharedFileCacheUseAfterOneFree tests whether a shared file cache is
// usable by a db, after one of the db's releases its reference.
func TestSharedFileCacheUseAfterOneFree(t *testing.T) {
	tc := newFileCacheTest(8<<20, 10, 1)
	db1, err := Open("test",
		&Options{
			FS:        vfs.NewMem(),
			Cache:     tc.cache,
			FileCache: tc,
		})
	require.NoError(t, err)

	// Release our reference, now that the db has a reference.
	tc.Unref()

	db2, err := Open("test",
		&Options{
			FS:        vfs.NewMem(),
			Cache:     tc.cache,
			FileCache: tc,
		})
	require.NoError(t, err)
	defer func() {
		require.NoError(t, db2.Close())
	}()

	// Make db1 release a reference to the cache. It should
	// still be usable by db2.
	require.NoError(t, db1.Close())
	v := tc.refs.Load()
	if v != 1 {
		t.Fatalf("expected reference count %d, got %d", 1, v)
	}

	// Check if db2 is still usable.
	start := []byte("a")
	end := []byte("d")
	require.NoError(t, db2.Set(start, nil, nil))
	require.NoError(t, db2.Flush())
	require.NoError(t, db2.DeleteRange(start, end, nil))
	require.NoError(t, db2.Compact(start, end, false))
}

// TestSharedFileCacheUsable ensures that a shared file cache is usable by more
// than one database at once.
func TestSharedFileCacheUsable(t *testing.T) {
	tc := newFileCacheTest(8<<20, 10, 1)
	db1, err := Open("test",
		&Options{
			FS:        vfs.NewMem(),
			Cache:     tc.cache,
			FileCache: tc,
		})
	require.NoError(t, err)

	// Release our reference, now that the db has a reference.
	tc.Unref()

	defer func() {
		require.NoError(t, db1.Close())
	}()

	db2, err := Open("test",
		&Options{
			FS:        vfs.NewMem(),
			Cache:     tc.cache,
			FileCache: tc,
		})
	require.NoError(t, err)
	defer func() {
		require.NoError(t, db2.Close())
	}()

	start := []byte("a")
	end := []byte("z")
	require.NoError(t, db1.Set(start, nil, nil))
	require.NoError(t, db1.Flush())
	require.NoError(t, db1.DeleteRange(start, end, nil))
	require.NoError(t, db1.Compact(start, end, false))

	start = []byte("x")
	end = []byte("y")
	require.NoError(t, db2.Set(start, nil, nil))
	require.NoError(t, db2.Flush())
	require.NoError(t, db2.Set(start, []byte{'a'}, nil))
	require.NoError(t, db2.Flush())
	require.NoError(t, db2.DeleteRange(start, end, nil))
	require.NoError(t, db2.Compact(start, end, false))
}

func TestSharedTableConcurrent(t *testing.T) {
	tc := newFileCacheTest(8<<20, 10, 1)
	db1, err := Open("test",
		&Options{
			FS:        vfs.NewMem(),
			Cache:     tc.cache,
			FileCache: tc,
		})
	require.NoError(t, err)

	// Release our reference, now that the db has a reference.
	tc.Unref()

	defer func() {
		require.NoError(t, db1.Close())
	}()

	db2, err := Open("test",
		&Options{
			FS:        vfs.NewMem(),
			Cache:     tc.cache,
			FileCache: tc,
		})
	require.NoError(t, err)
	defer func() {
		require.NoError(t, db2.Close())
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	// Now that both dbs have a reference to the file cache,
	// we'll run go routines which will use the DBs concurrently.
	concFunc := func(db *DB) {
		for i := 0; i < 1000; i++ {
			start := []byte("a")
			end := []byte("z")
			require.NoError(t, db.Set(start, nil, nil))
			require.NoError(t, db.Flush())
			require.NoError(t, db.DeleteRange(start, end, nil))
			require.NoError(t, db.Compact(start, end, false))
		}
		wg.Done()
	}

	go concFunc(db1)
	go concFunc(db2)

	wg.Wait()
}

func testFileCacheRandomAccess(t *testing.T, concurrent bool) {
	const N = 2000
	c, fs, err := newFileCacheContainerTest(nil, "")
	require.NoError(t, err)

	rngMu := sync.Mutex{}
	rng := rand.New(rand.NewPCG(1, 1))

	errc := make(chan error, N)
	for i := 0; i < N; i++ {
		go func(i int) {
			rngMu.Lock()
			fileNum, sleepTime := rng.IntN(fileCacheTestNumTables), rng.IntN(1000)
			rngMu.Unlock()
			m := &fileMetadata{FileNum: FileNum(fileNum)}
			m.InitPhysicalBacking()
			m.FileBacking.Ref()
			defer m.FileBacking.Unref()
			iters, err := c.newIters(context.Background(), m, nil, internalIterOpts{}, iterPointKeys)
			if err != nil {
				errc <- errors.Errorf("i=%d, fileNum=%d: find: %v", i, fileNum, err)
				return
			}
			iter := iters.Point()
			kv := iter.SeekGE([]byte("k"), base.SeekGEFlagsNone)
			if concurrent {
				time.Sleep(time.Duration(sleepTime) * time.Microsecond)
			}
			if kv == nil {
				errc <- errors.Errorf("i=%d, fileNum=%d: valid.0: got false, want true", i, fileNum)
				return
			}
			v, _, err := kv.Value(nil)
			if err != nil {
				errc <- errors.Errorf("i=%d, fileNum=%d: err extracting value: %v", err)
			}
			if got := len(v); got != fileNum {
				errc <- errors.Errorf("i=%d, fileNum=%d: value: got %d bytes, want %d", i, fileNum, got, fileNum)
				return
			}
			if kv := iter.Next(); kv != nil {
				errc <- errors.Errorf("i=%d, fileNum=%d: next.1: got true, want false", i, fileNum)
				return
			}
			if err := iter.Close(); err != nil {
				errc <- errors.Wrapf(err, "close error i=%d, fileNum=%dv", i, fileNum)
				return
			}
			errc <- nil
		}(i)
		if !concurrent {
			require.NoError(t, <-errc)
		}
	}
	if concurrent {
		for i := 0; i < N; i++ {
			require.NoError(t, <-errc)
		}
	}
	fs.validate(t, c, nil)
}

func TestFileCacheRandomAccessSequential(t *testing.T) { testFileCacheRandomAccess(t, false) }
func TestFileCacheRandomAccessConcurrent(t *testing.T) { testFileCacheRandomAccess(t, true) }

func testFileCacheFrequentlyUsedInternal(t *testing.T, rangeIter bool) {
	const (
		N       = 1000
		pinned0 = 7
		pinned1 = 11
	)
	c, fs, err := newFileCacheContainerTest(nil, "")
	require.NoError(t, err)

	for i := 0; i < N; i++ {
		for _, j := range [...]int{pinned0, i % fileCacheTestNumTables, pinned1} {
			var iters iterSet
			var err error
			m := &fileMetadata{FileNum: FileNum(j)}
			m.InitPhysicalBacking()
			m.FileBacking.Ref()
			if rangeIter {
				iters, err = c.newIters(context.Background(), m, nil, internalIterOpts{}, iterRangeKeys)
			} else {
				iters, err = c.newIters(context.Background(), m, nil, internalIterOpts{}, iterPointKeys)
			}
			if err != nil {
				t.Fatalf("i=%d, j=%d: find: %v", i, j, err)
			}
			if err := iters.CloseAll(); err != nil {
				t.Fatalf("i=%d, j=%d: close: %v", i, j, err)
			}
		}
	}

	fs.validate(t, c, func(i, gotO, gotC int) error {
		if i == pinned0 || i == pinned1 {
			if gotO != 1 || gotC != 0 {
				return errors.Errorf("i=%d: pinned table: got %d, %d, want %d, %d", i, gotO, gotC, 1, 0)
			}
		}
		return nil
	})
}

func TestFileCacheFrequentlyUsed(t *testing.T) {
	for i, iterType := range []string{"point", "range"} {
		t.Run(fmt.Sprintf("iter=%s", iterType), func(t *testing.T) {
			testFileCacheFrequentlyUsedInternal(t, i == 1)
		})
	}
}

func TestSharedFileCacheFrequentlyUsed(t *testing.T) {
	const (
		N       = 1000
		pinned0 = 7
		pinned1 = 11
	)
	tc := newFileCacheTest(8<<20, 2*fileCacheTestCacheSize, 16)
	c1, fs1, err := newFileCacheContainerTest(tc, "")
	require.NoError(t, err)
	c2, fs2, err := newFileCacheContainerTest(tc, "")
	require.NoError(t, err)
	tc.Unref()

	for i := 0; i < N; i++ {
		for _, j := range [...]int{pinned0, i % fileCacheTestNumTables, pinned1} {
			m := &fileMetadata{FileNum: FileNum(j)}
			m.InitPhysicalBacking()
			m.FileBacking.Ref()
			iters1, err := c1.newIters(context.Background(), m, nil, internalIterOpts{}, iterPointKeys)
			if err != nil {
				t.Fatalf("i=%d, j=%d: find: %v", i, j, err)
			}
			iters2, err := c2.newIters(context.Background(), m, nil, internalIterOpts{}, iterPointKeys)
			if err != nil {
				t.Fatalf("i=%d, j=%d: find: %v", i, j, err)
			}

			if err := iters1.point.Close(); err != nil {
				t.Fatalf("i=%d, j=%d: close: %v", i, j, err)
			}
			if err := iters2.point.Close(); err != nil {
				t.Fatalf("i=%d, j=%d: close: %v", i, j, err)
			}
		}
	}

	fs1.validate(t, c1, func(i, gotO, gotC int) error {
		if i == pinned0 || i == pinned1 {
			if gotO != 1 || gotC != 0 {
				return errors.Errorf("i=%d: pinned table: got %d, %d, want %d, %d", i, gotO, gotC, 1, 0)
			}
		}
		return nil
	})

	fs2.validate(t, c2, func(i, gotO, gotC int) error {
		if i == pinned0 || i == pinned1 {
			if gotO != 1 || gotC != 0 {
				return errors.Errorf("i=%d: pinned table: got %d, %d, want %d, %d", i, gotO, gotC, 1, 0)
			}
		}
		return nil
	})
}

func testFileCacheEvictionsInternal(t *testing.T, rangeIter bool) {
	const (
		N      = 1000
		lo, hi = 10, 20
	)
	c, fs, err := newFileCacheContainerTest(nil, "")
	require.NoError(t, err)

	rng := rand.New(rand.NewPCG(2, 2))
	for i := 0; i < N; i++ {
		j := rng.IntN(fileCacheTestNumTables)
		var iters iterSet
		var err error
		m := &fileMetadata{FileNum: FileNum(j)}
		m.InitPhysicalBacking()
		m.FileBacking.Ref()
		if rangeIter {
			iters, err = c.newIters(context.Background(), m, nil, internalIterOpts{}, iterRangeKeys)
		} else {
			iters, err = c.newIters(context.Background(), m, nil, internalIterOpts{}, iterPointKeys)
		}
		if err != nil {
			t.Fatalf("i=%d, j=%d: find: %v", i, j, err)
		}
		if err := iters.CloseAll(); err != nil {
			t.Fatalf("i=%d, j=%d: close: %v", i, j, err)
		}

		c.evict(base.DiskFileNum(lo + rng.Uint64N(hi-lo)))
	}

	sumEvicted, nEvicted := 0, 0
	sumSafe, nSafe := 0, 0
	fs.validate(t, c, func(i, gotO, gotC int) error {
		if lo <= i && i < hi {
			sumEvicted += gotO
			nEvicted++
		} else {
			sumSafe += gotO
			nSafe++
		}
		return nil
	})
	fEvicted := float64(sumEvicted) / float64(nEvicted)
	fSafe := float64(sumSafe) / float64(nSafe)
	// The magic 1.25 number isn't derived from formal modeling. It's just a guess. For
	// (lo, hi, fileCacheTestCacheSize, fileCacheTestNumTables) = (10, 20, 100, 300),
	// the ratio seems to converge on roughly 1.5 for large N, compared to 1.0 if we do
	// not evict any cache entries.
	if ratio := fEvicted / fSafe; ratio < 1.25 {
		t.Errorf("evicted tables were opened %.3f times on average, safe tables %.3f, ratio %.3f < 1.250",
			fEvicted, fSafe, ratio)
	}
}

func TestFileCacheEvictions(t *testing.T) {
	for i, iterType := range []string{"point", "range"} {
		t.Run(fmt.Sprintf("iter=%s", iterType), func(t *testing.T) {
			testFileCacheEvictionsInternal(t, i == 1)
		})
	}
}

func TestSharedFileCacheEvictions(t *testing.T) {
	const (
		N      = 1000
		lo, hi = 10, 20
	)
	tc := newFileCacheTest(8<<20, 2*fileCacheTestCacheSize, 16)
	c1, fs1, err := newFileCacheContainerTest(tc, "")
	require.NoError(t, err)
	c2, fs2, err := newFileCacheContainerTest(tc, "")
	require.NoError(t, err)
	tc.Unref()

	// TODO(radu): this test fails on most seeds.
	rng := rand.New(rand.NewPCG(0, 0))
	for i := 0; i < N; i++ {
		j := rng.IntN(fileCacheTestNumTables)
		m := &fileMetadata{FileNum: FileNum(j)}
		m.InitPhysicalBacking()
		m.FileBacking.Ref()
		iters1, err := c1.newIters(context.Background(), m, nil, internalIterOpts{}, iterPointKeys)
		if err != nil {
			t.Fatalf("i=%d, j=%d: find: %v", i, j, err)
		}

		iters2, err := c2.newIters(context.Background(), m, nil, internalIterOpts{}, iterPointKeys)
		if err != nil {
			t.Fatalf("i=%d, j=%d: find: %v", i, j, err)
		}

		if err := iters1.Point().Close(); err != nil {
			t.Fatalf("i=%d, j=%d: close: %v", i, j, err)
		}

		if err := iters2.Point().Close(); err != nil {
			t.Fatalf("i=%d, j=%d: close: %v", i, j, err)
		}

		c1.evict(base.DiskFileNum(lo + rng.Uint64N(hi-lo)))
		c2.evict(base.DiskFileNum(lo + rng.Uint64N(hi-lo)))
	}

	check := func(fs *fileCacheTestFS, c *fileCacheContainer) (float64, float64, float64) {
		sumEvicted, nEvicted := 0, 0
		sumSafe, nSafe := 0, 0
		fs.validate(t, c, func(i, gotO, gotC int) error {
			if lo <= i && i < hi {
				sumEvicted += gotO
				nEvicted++
			} else {
				sumSafe += gotO
				nSafe++
			}
			return nil
		})
		fEvicted := float64(sumEvicted) / float64(nEvicted)
		fSafe := float64(sumSafe) / float64(nSafe)

		return fEvicted, fSafe, fEvicted / fSafe
	}

	// The magic 1.25 number isn't derived from formal modeling. It's just a guess. For
	// (lo, hi, fileCacheTestCacheSize, fileCacheTestNumTables) = (10, 20, 100, 300),
	// the ratio seems to converge on roughly 1.5 for large N, compared to 1.0 if we do
	// not evict any cache entries.
	if fEvicted, fSafe, ratio := check(fs1, c1); ratio < 1.25 {
		t.Errorf(
			"evicted tables were opened %.3f times on average, safe tables %.3f, ratio %.3f < 1.250",
			fEvicted, fSafe, ratio,
		)
	}

	if fEvicted, fSafe, ratio := check(fs2, c2); ratio < 1.25 {
		t.Errorf(
			"evicted tables were opened %.3f times on average, safe tables %.3f, ratio %.3f < 1.250",
			fEvicted, fSafe, ratio,
		)
	}
}

func TestFileCacheIterLeak(t *testing.T) {
	c, _, err := newFileCacheContainerTest(nil, "")
	require.NoError(t, err)

	m := &fileMetadata{FileNum: 0}
	m.InitPhysicalBacking()
	m.FileBacking.Ref()
	defer m.FileBacking.Unref()
	iters, err := c.newIters(context.Background(), m, nil, internalIterOpts{}, iterPointKeys)
	require.NoError(t, err)

	if err := c.close(); err == nil {
		t.Fatalf("expected failure, but found success")
	} else if !strings.HasPrefix(err.Error(), "leaked iterators:") {
		t.Fatalf("expected leaked iterators, but found %+v", err)
	} else {
		t.Log(err.Error())
	}
	require.NoError(t, iters.Point().Close())
}

func TestSharedFileCacheIterLeak(t *testing.T) {
	tc := newFileCacheTest(8<<20, 2*fileCacheTestCacheSize, 16)
	c1, _, err := newFileCacheContainerTest(tc, "")
	require.NoError(t, err)
	c2, _, err := newFileCacheContainerTest(tc, "")
	require.NoError(t, err)
	c3, _, err := newFileCacheContainerTest(tc, "")
	require.NoError(t, err)
	tc.Unref()

	m := &fileMetadata{FileNum: 0}
	m.InitPhysicalBacking()
	m.FileBacking.Ref()
	defer m.FileBacking.Unref()
	iters, err := c1.newIters(context.Background(), m, nil, internalIterOpts{}, iterPointKeys)
	require.NoError(t, err)

	if err := c1.close(); err == nil {
		t.Fatalf("expected failure, but found success")
	} else if !strings.HasPrefix(err.Error(), "leaked iterators:") {
		t.Fatalf("expected leaked iterators, but found %+v", err)
	} else {
		t.Log(err.Error())
	}

	// Closing c2 shouldn't error out since c2 isn't leaking any iterators.
	require.NoError(t, c2.close())

	// Closing c3 should error out since c3 holds the last reference to the
	// FileCache, and when the FileCache closes, it will detect that there was a
	// leaked iterator.
	if err := c3.close(); err == nil {
		t.Fatalf("expected failure, but found success")
	} else if !strings.HasPrefix(err.Error(), "leaked iterators:") {
		t.Fatalf("expected leaked iterators, but found %+v", err)
	} else {
		t.Log(err.Error())
	}

	require.NoError(t, iters.Point().Close())
}

func TestFileCacheRetryAfterFailure(t *testing.T) {
	// Test a retry can succeed after a failure, i.e., errors are not cached.
	c, fs, err := newFileCacheContainerTest(nil, "")
	require.NoError(t, err)

	fs.setOpenError(true /* enabled */)
	m := &fileMetadata{FileNum: 0}
	m.InitPhysicalBacking()
	m.FileBacking.Ref()
	defer m.FileBacking.Unref()
	if _, err = c.newIters(context.Background(), m, nil, internalIterOpts{}, iterPointKeys); err == nil {
		t.Fatalf("expected failure, but found success")
	}
	require.Equal(t, "pebble: backing file 000000 error: injected error", err.Error())
	fs.setOpenError(false /* enabled */)
	var iters iterSet
	iters, err = c.newIters(context.Background(), m, nil, internalIterOpts{}, iterPointKeys)
	require.NoError(t, err)
	require.NoError(t, iters.Point().Close())
	fs.validate(t, c, nil)
}

func TestFileCacheErrorBadMagicNumber(t *testing.T) {
	obj := &objstorage.MemObj{}
	tw := sstable.NewWriter(obj, sstable.WriterOptions{TableFormat: sstable.TableFormatPebblev2})
	tw.Set([]byte("a"), nil)
	require.NoError(t, tw.Close())
	buf := obj.Data()
	// Bad magic number.
	buf[len(buf)-1] = 0
	fs := &fileCacheTestFS{
		FS: vfs.NewMem(),
	}
	const testFileNum = 3
	objProvider, err := objstorageprovider.Open(objstorageprovider.DefaultSettings(fs, ""))
	require.NoError(t, err)
	w, _, err := objProvider.Create(context.Background(), fileTypeTable, testFileNum, objstorage.CreateOptions{})
	w.Write(buf)
	require.NoError(t, w.Finish())
	opts := &Options{}
	opts.EnsureDefaults()
	opts.Cache = NewCache(8 << 20) // 8 MB
	defer opts.Cache.Unref()
	c := newFileCacheContainer(nil, opts.Cache.NewID(), objProvider, opts, fileCacheTestCacheSize,
		&sstable.CategoryStatsCollector{})
	require.NoError(t, err)
	defer c.close()

	m := &fileMetadata{FileNum: testFileNum}
	m.InitPhysicalBacking()
	m.FileBacking.Ref()
	defer m.FileBacking.Unref()
	if _, err = c.newIters(context.Background(), m, nil, internalIterOpts{}, iterPointKeys); err == nil {
		t.Fatalf("expected failure, but found success")
	}
	require.Equal(t,
		"pebble: backing file 000003 error: pebble/table: invalid table 000003: (bad magic number: 0xf09faab3f09faa00)",
		err.Error())
}

func TestFileCacheEvictClose(t *testing.T) {
	errs := make(chan error, 10)
	db, err := Open("test",
		&Options{
			FS: vfs.NewMem(),
			EventListener: &EventListener{
				TableDeleted: func(info TableDeleteInfo) {
					errs <- info.Err
				},
			},
		})
	require.NoError(t, err)

	start := []byte("a")
	end := []byte("z")
	require.NoError(t, db.Set(start, nil, nil))
	require.NoError(t, db.Flush())
	require.NoError(t, db.DeleteRange(start, end, nil))
	require.NoError(t, db.Compact(start, end, false))
	require.NoError(t, db.Close())
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
}

func TestFileCacheClockPro(t *testing.T) {
	// Test data was generated from the python code. See also
	// internal/cache/clockpro_test.go:TestCache.
	f, err := os.Open("internal/cache/testdata/cache")
	require.NoError(t, err)

	mem := vfs.NewMem()
	objProvider, err := objstorageprovider.Open(objstorageprovider.DefaultSettings(mem, ""))
	require.NoError(t, err)
	defer objProvider.Close()

	makeTable := func(dfn base.DiskFileNum) {
		require.NoError(t, err)
		f, _, err := objProvider.Create(context.Background(), fileTypeTable, dfn, objstorage.CreateOptions{})
		require.NoError(t, err)
		w := sstable.NewWriter(f, sstable.WriterOptions{})
		require.NoError(t, w.Set([]byte("a"), nil))
		require.NoError(t, w.Close())
	}

	opts := &Options{
		Cache: NewCache(8 << 20), // 8 MB
	}
	opts.EnsureDefaults()
	defer opts.Cache.Unref()

	cache := &fileCacheShard{}
	// NB: The file cache size of 200 is required for the expected test values.
	cache.init(200)
	dbOpts := &fileCacheOpts{}
	dbOpts.loggerAndTracer = &base.LoggerWithNoopTracer{Logger: opts.Logger}
	dbOpts.cacheID = 0
	dbOpts.objProvider = objProvider
	dbOpts.readerOpts = opts.MakeReaderOptions()

	scanner := bufio.NewScanner(f)
	tables := make(map[int]bool)
	line := 1

	for scanner.Scan() {
		fields := bytes.Fields(scanner.Bytes())

		key, err := strconv.Atoi(string(fields[0]))
		require.NoError(t, err)

		// Ensure that underlying sstables exist on disk, creating each table the
		// first time it is seen.
		if !tables[key] {
			makeTable(base.DiskFileNum(key))
			tables[key] = true
		}

		oldHits := cache.hits.Load()
		m := &fileMetadata{FileNum: FileNum(key)}
		m.InitPhysicalBacking()
		m.FileBacking.Ref()
		v := cache.findNode(context.Background(), m.FileBacking, dbOpts)
		cache.unrefValue(v)

		hit := cache.hits.Load() != oldHits
		wantHit := fields[1][0] == 'h'
		if hit != wantHit {
			t.Errorf("%d: cache hit mismatch: got %v, want %v\n", line, hit, wantHit)
		}
		line++
		m.FileBacking.Unref()
	}
}

func BenchmarkNewItersAlloc(b *testing.B) {
	opts := &Options{
		FS:                 vfs.NewMem(),
		FormatMajorVersion: internalFormatNewest,
	}
	d, err := Open("", opts)
	require.NoError(b, err)
	defer func() { require.NoError(b, d.Close()) }()

	require.NoError(b, d.Set([]byte{'a'}, []byte{'a'}, nil))
	require.NoError(b, d.Flush())
	require.NoError(b, d.Compact([]byte{'a'}, []byte{'z'}, false))

	d.mu.Lock()
	currVersion := d.mu.versions.currentVersion()
	it := currVersion.Levels[6].Iter()
	m := it.First()
	require.NotNil(b, m)
	d.mu.Unlock()

	// Open once so that the Reader is cached.
	iters, err := d.newIters(context.Background(), m, nil, internalIterOpts{}, iterPointKeys|iterRangeDeletions)
	require.NoError(b, iters.CloseAll())
	require.NoError(b, err)

	for i := 0; i < b.N; i++ {
		b.StartTimer()
		iters, err := d.newIters(context.Background(), m, nil, internalIterOpts{}, iterPointKeys|iterRangeDeletions)
		b.StopTimer()
		require.NoError(b, err)
		require.NoError(b, iters.CloseAll())
	}
}

// TestFileCacheNoSuchFileError verifies that when the file cache hits a "no
// such file" error, it generates a useful fatal message.
func TestFileCacheNoSuchFileError(t *testing.T) {
	const dirname = "test"
	mem := vfs.NewMem()
	logger := &catchFatalLogger{}

	d, err := Open(dirname, &Options{
		FS:     mem,
		Logger: logger,
	})
	require.NoError(t, err)
	defer func() { _ = d.Close() }()
	require.NoError(t, d.Set([]byte("a"), []byte("val_a"), nil))
	require.NoError(t, d.Set([]byte("b"), []byte("val_b"), nil))
	require.NoError(t, d.Flush())
	ls, err := mem.List(dirname)
	require.NoError(t, err)

	// Find the sst file.
	var sst string
	for _, file := range ls {
		if strings.HasSuffix(file, ".sst") {
			if sst != "" {
				t.Fatalf("multiple SSTs found: %s, %s", sst, file)
			}
			sst = file
		}
	}
	if sst == "" {
		t.Fatalf("no SST found after flush")
	}
	require.NoError(t, mem.Remove(path.Join(dirname, sst)))

	_, _, _ = d.Get([]byte("a"))
	require.NotZero(t, len(logger.fatalMsgs), "no fatal message emitted")
	require.Equal(t, 1, len(logger.fatalMsgs), "expected one fatal message; got: %v", logger.fatalMsgs)
	require.Contains(t, logger.fatalMsgs[0], "directory contains 7 files, 2 unknown, 0 tables, 2 logs, 1 manifests")
}

func BenchmarkFileCacheHotPath(b *testing.B) {
	mem := vfs.NewMem()
	objProvider, err := objstorageprovider.Open(objstorageprovider.DefaultSettings(mem, ""))
	require.NoError(b, err)
	defer objProvider.Close()

	makeTable := func(dfn base.DiskFileNum) {
		require.NoError(b, err)
		f, _, err := objProvider.Create(context.Background(), fileTypeTable, dfn, objstorage.CreateOptions{})
		require.NoError(b, err)
		w := sstable.NewWriter(f, sstable.WriterOptions{})
		require.NoError(b, w.Set([]byte("a"), nil))
		require.NoError(b, w.Close())
	}

	opts := &Options{
		Cache: NewCache(8 << 20), // 8 MB
	}
	opts.EnsureDefaults()
	defer opts.Cache.Unref()

	cache := &fileCacheShard{}
	cache.init(2)
	dbOpts := &fileCacheOpts{}
	dbOpts.loggerAndTracer = &base.LoggerWithNoopTracer{Logger: opts.Logger}
	dbOpts.cacheID = 0
	dbOpts.objProvider = objProvider
	dbOpts.readerOpts = opts.MakeReaderOptions()

	makeTable(1)

	m := &fileMetadata{FileNum: 1}
	m.InitPhysicalBacking()
	m.FileBacking.Ref()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v := cache.findNode(context.Background(), m.FileBacking, dbOpts)
		cache.unrefValue(v)
	}
}

type catchFatalLogger struct {
	fatalMsgs []string
}

var _ Logger = (*catchFatalLogger)(nil)

func (tl *catchFatalLogger) Infof(format string, args ...interface{})  {}
func (tl *catchFatalLogger) Errorf(format string, args ...interface{}) {}

func (tl *catchFatalLogger) Fatalf(format string, args ...interface{}) {
	tl.fatalMsgs = append(tl.fatalMsgs, fmt.Sprintf(format, args...))
}