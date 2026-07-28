// Copyright 2017 The go-sqlite Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sqlite3

import (
	"encoding/binary"
	"io"
	"os"
)

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
)

// walIndex locates the most recent committed image of each page in a WAL.
type walIndex struct {
	// f is the log the offsets point into. readWALIndex leaves it nil, since
	// it indexes any reader; openWAL sets it to the file it opened.
	f *os.File
	// offsets maps a page number to the offset of its page data in the WAL.
	offsets map[int]int64
	// dbSize is the size of the database in pages after the last commit frame,
	// which supersedes the size recorded in the main file's header.
	dbSize int
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

	var bigEndian bool
	switch binary.BigEndian.Uint32(header[0:4]) {
	case walMagicLittleEndian:
		bigEndian = false
	case walMagicBigEndian:
		bigEndian = true
	default:
		return nil, nil
	}

	// A torn or reset header means there is no snapshot to read.
	s0, s1 := walChecksum(bigEndian, 0, 0, header[0:24])
	if s0 != binary.BigEndian.Uint32(header[24:28]) || s1 != binary.BigEndian.Uint32(header[28:32]) {
		return nil, nil
	}
	if int(binary.BigEndian.Uint32(header[8:12])) != pageSize {
		return nil, nil
	}
	salt := header[16:24]

	index := &walIndex{offsets: make(map[int]int64)}
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
		if string(frame[8:16]) != string(salt) {
			break
		}
		c0, c1 := walChecksum(bigEndian, s0, s1, frame[0:8])
		c0, c1 = walChecksum(bigEndian, c0, c1, frame[walFrameHeaderSize:])
		if c0 != binary.BigEndian.Uint32(frame[16:20]) || c1 != binary.BigEndian.Uint32(frame[20:24]) {
			break
		}
		s0, s1 = c0, c1

		pending[int(binary.BigEndian.Uint32(frame[0:4]))] = offset + walFrameHeaderSize
		if dbSize := binary.BigEndian.Uint32(frame[4:8]); dbSize != 0 {
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
// whole number of 8-byte blocks.
func walChecksum(bigEndian bool, s0, s1 uint32, b []byte) (uint32, uint32) {
	order := binary.ByteOrder(binary.LittleEndian)
	if bigEndian {
		order = binary.BigEndian
	}
	for i := 0; i+8 <= len(b); i += 8 {
		s0 += order.Uint32(b[i:i+4]) + s1
		s1 += order.Uint32(b[i+4:i+8]) + s0
	}
	return s0, s1
}

// openWAL indexes the write-ahead log next to the database at dbPath,
// returning a nil index when there is no snapshot to read. The caller owns the
// returned index's file and must close it.
//
// A log that cannot be opened at all is treated as absent, since a permission
// denial or a sharing violation on it says nothing about the main file, which
// stays perfectly readable. An I/O fault on a log already open says the
// opposite, so those errors propagate rather than silently serving stale pages
// in place of a snapshot that is really there.
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
