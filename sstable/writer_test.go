// Copyright 2018 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package sstable

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/cache"
	"github.com/cockroachdb/pebble/internal/datadriven"
	"github.com/cockroachdb/pebble/internal/rangedel"
	"github.com/cockroachdb/pebble/vfs"
)

func TestWriter(t *testing.T) {
	var r *Reader
	datadriven.RunTest(t, "testdata/writer", func(td *datadriven.TestData) string {
		switch td.Cmd {
		case "build":
			if r != nil {
				_ = r.Close()
				r = nil
			}

			mem := vfs.NewMem()
			f0, err := mem.Create("test")
			if err != nil {
				return err.Error()
			}

			var writerOpts WriterOptions
			for _, arg := range td.CmdArgs {
				switch arg.Key {
				case "leveldb":
					if len(arg.Vals) != 0 {
						return fmt.Sprintf("%s: arg %s expects 0 values", td.Cmd, arg.Key)
					}
					writerOpts.TableFormat = TableFormatLevelDB
				case "index-block-size":
					if len(arg.Vals) != 1 {
						return fmt.Sprintf("%s: arg %s expects 1 value", td.Cmd, arg.Key)
					}
					var err error
					writerOpts.IndexBlockSize, err = strconv.Atoi(arg.Vals[0])
					if err != nil {
						return err.Error()
					}
				default:
					return fmt.Sprintf("%s: unknown arg %s", td.Cmd, arg.Key)
				}
			}

			w := NewWriter(f0, writerOpts)
			var tombstones []rangedel.Tombstone
			f := rangedel.Fragmenter{
				Cmp: DefaultComparer.Compare,
				Emit: func(fragmented []rangedel.Tombstone) {
					tombstones = append(tombstones, fragmented...)
				},
			}
			for _, data := range strings.Split(td.Input, "\n") {
				j := strings.Index(data, ":")
				key := base.ParseInternalKey(data[:j])
				value := []byte(data[j+1:])
				switch key.Kind() {
				case InternalKeyKindRangeDelete:
					var err error
					func() {
						defer func() {
							if r := recover(); r != nil {
								err = errors.New(fmt.Sprint(r))
							}
						}()
						f.Add(key, value)
					}()
					if err != nil {
						return err.Error()
					}
				default:
					if err := w.Add(key, value); err != nil {
						return err.Error()
					}

				}
			}
			f.Finish()
			for _, v := range tombstones {
				if err := w.Add(v.Start, v.End); err != nil {
					return err.Error()
				}
			}
			if err := w.Close(); err != nil {
				return err.Error()
			}
			meta, err := w.Metadata()
			if err != nil {
				return err.Error()
			}

			f1, err := mem.Open("test")
			if err != nil {
				return err.Error()
			}
			r, err = NewReader(f1, ReaderOptions{})
			if err != nil {
				return err.Error()
			}
			return fmt.Sprintf("point:   [%s,%s]\nrange:   [%s,%s]\nseqnums: [%d,%d]\n",
				meta.SmallestPoint, meta.LargestPoint,
				meta.SmallestRange, meta.LargestRange,
				meta.SmallestSeqNum, meta.LargestSeqNum)

		case "build-raw":
			if r != nil {
				_ = r.Close()
				r = nil
			}

			mem := vfs.NewMem()
			f0, err := mem.Create("test")
			if err != nil {
				return err.Error()
			}

			w := NewWriter(f0, WriterOptions{})
			for i := range td.CmdArgs {
				arg := &td.CmdArgs[i]
				if arg.Key == "range-del-v1" {
					w.rangeDelV1Format = true
					break
				}
			}

			for _, data := range strings.Split(td.Input, "\n") {
				j := strings.Index(data, ":")
				key := base.ParseInternalKey(data[:j])
				value := []byte(data[j+1:])
				if err := w.Add(key, value); err != nil {
					return err.Error()
				}
			}
			if err := w.Close(); err != nil {
				return err.Error()
			}
			meta, err := w.Metadata()
			if err != nil {
				return err.Error()
			}

			f1, err := mem.Open("test")
			if err != nil {
				return err.Error()
			}
			r, err = NewReader(f1, ReaderOptions{})
			if err != nil {
				return err.Error()
			}
			return fmt.Sprintf("point:   [%s,%s]\nrange:   [%s,%s]\nseqnums: [%d,%d]\n",
				meta.SmallestPoint, meta.LargestPoint,
				meta.SmallestRange, meta.LargestRange,
				meta.SmallestSeqNum, meta.LargestSeqNum)

		case "scan":
			iter := newIterAdapter(r.NewIter(nil /* lower */, nil /* upper */))
			defer iter.Close()

			var buf bytes.Buffer
			for valid := iter.First(); valid; valid = iter.Next() {
				fmt.Fprintf(&buf, "%s:%s\n", iter.Key(), iter.Value())
			}
			return buf.String()

		case "scan-range-del":
			iter := r.NewRangeDelIter()
			if iter == nil {
				return ""
			}
			defer iter.Close()

			var buf bytes.Buffer
			for key, val := iter.First(); key != nil; key, val = iter.Next() {
				fmt.Fprintf(&buf, "%s:%s\n", key, val)
			}
			return buf.String()

		case "layout":
			l, err := r.Layout()
			if err != nil {
				return err.Error()
			}
			var buf bytes.Buffer
			l.Describe(&buf, false, r, nil)
			return buf.String()

		default:
			return fmt.Sprintf("unknown command: %s", td.Cmd)
		}
	})
}

func TestWriterClearCache(t *testing.T) {
	// Verify that Writer clears the cache of blocks that it writes.
	mem := vfs.NewMem()
	opts := ReaderOptions{Cache: cache.New(64 << 20)}
	writerOpts := WriterOptions{Cache: opts.Cache}
	cacheOpts := &cacheOpts{cacheID: 1, fileNum: 1}
	invalidData := []byte("invalid data")

	build := func(name string) {
		f, err := mem.Create(name)
		if err != nil {
			t.Fatal(err)
		}

		w := NewWriter(f, writerOpts, cacheOpts)
		if err := w.Set([]byte("hello"), []byte("world")); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// Build the sstable a first time so that we can determine the locations of
	// all of the blocks.
	build("test")

	f, err := mem.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(f, opts)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := r.Layout()
	if err != nil {
		t.Fatal(err)
	}

	foreachBH := func(layout *Layout, f func(bh BlockHandle)) {
		for _, bh := range layout.Data {
			f(bh)
		}
		for _, bh := range layout.Index {
			f(bh)
		}
		f(layout.TopIndex)
		f(layout.Filter)
		f(layout.RangeDel)
		f(layout.Properties)
		f(layout.MetaIndex)
	}

	// Poison the cache for each of the blocks.
	poison := func(bh BlockHandle) {
		opts.Cache.Set(cacheOpts.cacheID, cacheOpts.fileNum, bh.Offset, invalidData)
	}
	foreachBH(layout, poison)

	// Build the table a second time. This should clear the cache for the blocks
	// that are written.
	build("test")

	// Verify that the written blocks have been cleared from the cache.
	check := func(bh BlockHandle) {
		h := opts.Cache.Get(cacheOpts.cacheID, cacheOpts.fileNum, bh.Offset)
		if h.Get() != nil {
			t.Fatalf("%d: expected cache to be cleared, but found %q", bh.Offset, invalidData)
		}
	}
	foreachBH(layout, check)
}
