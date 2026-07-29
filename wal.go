// Copyright 2017 The go-sqlite Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sqlite3

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
)

// errWALChanged reports that the log was checkpointed and restarted after it
// was indexed, so the offsets no longer describe it. Reopening the database
// picks up the new generation.
var errWALChanged = errors.New("sqlite3: write-ahead log changed while being read")

// Write-ahead log reader.
//
// A database in WAL mode keeps recent pages in a separate "-wal" file and only
// folds them back into the main file at a checkpoint. Reading the main file
// alone therefore returns whatever the last checkpoint left behind, which for a
// database belonging to a running application can be arbitrarily stale.
//
// Format: https://sqlite.org/fileformat2.html#walformat
const (
	walHeaderSize      = 32
	walFrameHeaderSize = 24

	// Low bit set means the checksums are big-endian, per wal.c's
	// SQLITE_BIGENDIAN test. The format page's prose reads the other way round.
	walMagicLittleEndian = 0x377f0682
	walMagicBigEndian    = 0x377f0683

	// The only format version wal.c writes or accepts.
	walFormatVersion = 3007000
)

// walIndex locates the most recent committed image of each page in a WAL.
type walIndex struct {
	// f is the log the offsets point into; nil until openWAL sets it
	// (readWALIndex indexes any reader), so every index reaching a pager has one.
	f *os.File
	// offsets maps a page number to the offset of its frame in the WAL.
	offsets map[int]int64
	// salt is the header's, repeated in every frame belonging to this
	// generation of the log.
	salt [8]byte
	// dbSize is the size of the database in pages after the last commit frame,
	// which supersedes the size recorded in the main file's header.
	dbSize int
}

// page reads the log's image of page i into buf, reporting whether the log
// carries one. A page it does not carry is left to the main database file.
func (w *walIndex) page(i int, buf []byte) (bool, error) {
	off, ok := w.offsets[i]
	if !ok {
		return false, nil
	}
	// A checkpoint can restart the log between the scan and this read, leaving
	// the offset pointing into a later generation's frame, or past the end of a
	// log that was truncated. Both are caught here, all but a restart that
	// draws the same salt, which would need the checksum chain rewalked per
	// read to see.
	var header [walFrameHeaderSize]byte
	if _, err := w.f.ReadAt(header[:], off); err != nil {
		return true, walReadError(err)
	}
	if binary.BigEndian.Uint32(header[0:4]) != uint32(i) || string(header[8:16]) != string(w.salt[:]) {
		return true, errWALChanged
	}
	if _, err := w.f.ReadAt(buf, off+walFrameHeaderSize); err != nil {
		return true, walReadError(err)
	}
	return true, nil
}

// walReadError reports a short read at an offset the scan already reached as a
// restart rather than as an I/O fault, since only a truncation can shorten the
// log and that is how a checkpoint restarts it.
func walReadError(err error) error {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return errWALChanged
	}
	return err
}

func (w *walIndex) Close() error {
	return w.f.Close()
}

// readWALIndex scans the WAL in f and indexes every page belonging to a
// committed transaction, newest image winning. It returns a nil index for a
// log SQLite would itself ignore, such as the stale bytes a checkpoint leaves
// behind; only I/O failures come back as errors.
func readWALIndex(f io.ReaderAt, pageSize int) (*walIndex, error) {
	header := make([]byte, walHeaderSize)
	if _, err := f.ReadAt(header, 0); err != nil {
		if err == io.EOF {
			return nil, nil
		}
		return nil, err
	}

	var order binary.ByteOrder = binary.LittleEndian
	switch binary.BigEndian.Uint32(header[0:4]) {
	case walMagicLittleEndian:
	case walMagicBigEndian:
		order = binary.BigEndian
	default:
		return nil, nil
	}
	// A later format may keep the magic and reuse these fields differently.
	if binary.BigEndian.Uint32(header[4:8]) != walFormatVersion {
		return nil, nil
	}

	// A torn or reset header means there is no snapshot to read.
	s0, s1 := walChecksum(order, 0, 0, header[0:24])
	if s0 != binary.BigEndian.Uint32(header[24:28]) || s1 != binary.BigEndian.Uint32(header[28:32]) {
		return nil, nil
	}
	// The pager reads fixed-size pages, so a log written at another page size
	// is no more usable here than an absent one.
	if int(binary.BigEndian.Uint32(header[8:12])) != pageSize {
		return nil, nil
	}

	index := &walIndex{offsets: make(map[int]int64)}
	copy(index.salt[:], header[16:24])
	// Frames after the last commit frame belong to a transaction that was never
	// committed, so they are staged here and only merged when a commit is seen.
	pending := make(map[int]int64)

	frame := make([]byte, walFrameHeaderSize+pageSize)
	for offset := int64(walHeaderSize); ; offset += int64(len(frame)) {
		if _, err := f.ReadAt(frame, offset); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		// A salt mismatch marks where a later checkpoint restarted the log and
		// left older frames behind.
		if string(frame[8:16]) != string(index.salt[:]) {
			break
		}
		c0, c1 := walChecksum(order, s0, s1, frame[0:8])
		c0, c1 = walChecksum(order, c0, c1, frame[walFrameHeaderSize:])
		if c0 != binary.BigEndian.Uint32(frame[16:20]) || c1 != binary.BigEndian.Uint32(frame[20:24]) {
			break
		}
		s0, s1 = c0, c1

		// Page 0 does not exist; wal.c rejects such a frame outright.
		pgno := binary.BigEndian.Uint32(frame[0:4])
		if pgno == 0 {
			break
		}
		dbSize := binary.BigEndian.Uint32(frame[4:8])
		if dbSize > math.MaxInt32 {
			break
		}

		pending[int(pgno)] = offset
		if dbSize != 0 {
			for page, at := range pending {
				index.offsets[page] = at
			}
			pending = make(map[int]int64)
			index.dbSize = int(dbSize)
		}
	}

	if len(index.offsets) == 0 {
		return nil, nil
	}
	return index, nil
}

// walChecksum continues SQLite's running WAL checksum over b, which must be a
// whole number of 8-byte blocks. The log records its own word order.
func walChecksum(order binary.ByteOrder, s0, s1 uint32, b []byte) (uint32, uint32) {
	for i := 0; i+8 <= len(b); i += 8 {
		s0 += order.Uint32(b[i:i+4]) + s1
		s1 += order.Uint32(b[i+4:i+8]) + s0
	}
	return s0, s1
}

// openWAL indexes the write-ahead log next to the database at dbPath, returning
// a nil index when there is no snapshot to read; the caller owns the returned
// index's file and must close it.
//
// A log that cannot be opened is treated as absent (a permission or sharing
// error there says nothing about the main file), but an I/O fault on a log
// already open propagates rather than silently serving stale pages.
func openWAL(dbPath string, pageSize int) (*walIndex, error) {
	f, err := os.Open(dbPath + "-wal")
	if err != nil {
		return nil, nil
	}
	index, err := readWALIndex(f, pageSize)
	if err != nil || index == nil {
		f.Close()
		return nil, err
	}
	index.f = f
	return index, nil
}
