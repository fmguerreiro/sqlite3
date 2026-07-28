// Copyright 2017 The go-sqlite Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sqlite3

import (
	"encoding/binary"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// testdata/wal.sqlite is a database left in WAL mode by a still-open
// connection. Its main file was checkpointed after the first row only, so
// reading it alone yields one row and user_version 7; the log carries two more
// rows and user_version 16.
//
// Do not open the fixture with the sqlite3 CLI or any writing client: that
// checkpoints the log into the main file and empties it, which is precisely
// the state the fixture exists to not be in. TestOpenWithoutWAL is what
// catches it, by way of reporting three rows where it wanted one.
const (
	walFixture          = "testdata/wal.sqlite"
	walStaleUserVersion = 7
	walUserVersion      = 16
)

var (
	// walStaleRows is what the main file holds on its own, walRows what the
	// log adds to it.
	walStaleRows = []string{"checkpointed"}
	walRows      = []string{"checkpointed", "in-wal-a", "in-wal-b"}
)

func rowsInTbl1(t *testing.T, db *DbFile) []string {
	t.Helper()
	var got []string
	err := db.VisitTableRecords("tbl1", func(_ *int64, rec Record) error {
		got = append(got, rec.Values[0].(string))
		return nil
	})
	if err != nil {
		t.Fatalf("visiting tbl1: %v", err)
	}
	return got
}

// copyDB copies the fixture database into a temporary directory, taking the
// log with it only when withWAL is set. Tests mutate the copy, never the
// fixture. The returned func removes the directory.
func copyDB(t *testing.T, withWAL bool) (string, func()) {
	t.Helper()
	dir, err := ioutil.TempDir("", "sqlite3-wal")
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "wal.sqlite")
	copyFile(t, walFixture, dst)
	if withWAL {
		copyFile(t, walFixture+"-wal", dst+"-wal")
	}
	return dst, func() { os.RemoveAll(dir) }
}

// assertRows opens path and checks tbl1 holds exactly want.
func assertRows(t *testing.T, path string, want []string) {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if got := rowsInTbl1(t, db); !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %q, want %q", got, want)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := ioutil.ReadFile(src)
	if err != nil {
		t.Fatalf("reading %s: %v", src, err)
	}
	if err := ioutil.WriteFile(dst, b, 0644); err != nil {
		t.Fatalf("writing %s: %v", dst, err)
	}
}

func TestOpenWithWAL(t *testing.T) {
	db, err := Open(walFixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if got := rowsInTbl1(t, db); !reflect.DeepEqual(got, walRows) {
		t.Errorf("rows = %q, want %q; the write-ahead log was not applied", got, walRows)
	}
	// The log also carries a newer page 1, so the header must come from it.
	if got := db.UserVersion(); got != walUserVersion {
		t.Errorf("UserVersion() = %d, want %d", got, walUserVersion)
	}
}

// Without the log beside it the same main file is a valid, older database.
func TestOpenWithoutWAL(t *testing.T) {
	path, cleanup := copyDB(t, false)
	defer cleanup()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if got := rowsInTbl1(t, db); !reflect.DeepEqual(got, walStaleRows) {
		t.Errorf("rows = %q, want %q", got, walStaleRows)
	}
	if got := db.UserVersion(); got != walStaleUserVersion {
		t.Errorf("UserVersion() = %d, want %d", got, walStaleUserVersion)
	}
}

// A reader without a path to work from cannot find the log, and must still read
// the main file rather than fail.
func TestOpenFromUnnamedIgnoresWAL(t *testing.T) {
	f, err := os.Open(walFixture)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	db, err := OpenFrom(struct{ readSeeker }{f})
	if err != nil {
		t.Fatalf("OpenFrom: %v", err)
	}
	defer db.Close()

	if got := rowsInTbl1(t, db); !reflect.DeepEqual(got, walStaleRows) {
		t.Errorf("rows = %q, want %q", got, walStaleRows)
	}
}

// readSeeker hides *os.File's Name method so OpenFrom sees an anonymous reader.
type readSeeker interface {
	Read([]byte) (int, error)
	Seek(int64, int) (int64, error)
}

// A torn frame can appear at the end of any log a writer is still appending
// to, and must be dropped without taking the committed snapshot with it.
func TestWALStopsAtTornTail(t *testing.T) {
	path, cleanup := copyDB(t, true)
	defer cleanup()
	header, err := ioutil.ReadFile(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(path+"-wal", os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatal(err)
	}
	// Right size, right salt, so only the checksum can reject it. A frame
	// carrying the wrong salt would be turned away one check earlier.
	torn := make([]byte, walFrameHeaderSize+1024)
	copy(torn[8:16], header[16:24])
	if _, err := log.Write(torn); err != nil {
		t.Fatal(err)
	}
	log.Close()

	assertRows(t, path, walRows)
}

// A log whose header does not checksum is one SQLite would ignore — typically a
// log reset by a checkpoint — and it must not make the database unreadable.
func TestWALWithBadHeaderIgnored(t *testing.T) {
	path, cleanup := copyDB(t, true)
	defer cleanup()
	log, err := os.OpenFile(path+"-wal", os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the salt, which the header checksum covers.
	if _, err := log.WriteAt([]byte{0xff, 0xff, 0xff, 0xff}, 16); err != nil {
		t.Fatal(err)
	}
	log.Close()

	assertRows(t, path, walStaleRows)
}

// openWAL treats a log it cannot read as absent, so the main file still reads.
func TestWALUnreadableIgnored(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads the log regardless of its mode")
	}
	path, cleanup := copyDB(t, true)
	defer cleanup()
	if err := os.Chmod(path+"-wal", 0); err != nil {
		t.Fatal(err)
	}

	assertRows(t, path, walStaleRows)
}

// SQLite writes a log's checksums in the byte order of the machine that
// created it, so a log from a big-endian host uses the other magic number and
// the other word order. There is no fixture from such a host to hand, so this
// rewrites the little-endian one into that form, computing the checksums here
// rather than through walChecksum so that transposing the two magic constants
// fails the test instead of cancelling out.
func TestWALBigEndianChecksums(t *testing.T) {
	path, cleanup := copyDB(t, true)
	defer cleanup()
	log, err := ioutil.ReadFile(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}

	sum := func(s0, s1 uint32, b []byte) (uint32, uint32) {
		for i := 0; i+8 <= len(b); i += 8 {
			s0 += binary.BigEndian.Uint32(b[i:i+4]) + s1
			s1 += binary.BigEndian.Uint32(b[i+4:i+8]) + s0
		}
		return s0, s1
	}

	binary.BigEndian.PutUint32(log[0:4], walMagicBigEndian)
	s0, s1 := sum(0, 0, log[0:24])
	binary.BigEndian.PutUint32(log[24:28], s0)
	binary.BigEndian.PutUint32(log[28:32], s1)

	frame := walFrameHeaderSize + 1024
	for off := walHeaderSize; off+frame <= len(log); off += frame {
		f := log[off : off+frame]
		s0, s1 = sum(s0, s1, f[0:8])
		s0, s1 = sum(s0, s1, f[walFrameHeaderSize:])
		binary.BigEndian.PutUint32(f[16:20], s0)
		binary.BigEndian.PutUint32(f[20:24], s1)
	}
	if err := ioutil.WriteFile(path+"-wal", log, 0644); err != nil {
		t.Fatal(err)
	}

	assertRows(t, path, walRows)
}

// A log can shrink the database as well as grow it, so the page count has to
// come from the commit frame rather than the main file's header. In
// testdata/wal-shrink.sqlite an auto-vacuuming database dropped 60 rows in the
// logged transaction, taking it from 34 pages to 4.
func TestWALShrinksDatabase(t *testing.T) {
	db, err := Open("testdata/wal-shrink.sqlite")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if got, want := db.NumPage(), 4; got != want {
		t.Errorf("NumPage() = %d, want %d", got, want)
	}
	if _, err := db.pager.Page(5); err == nil {
		t.Error("Page(5) succeeded past the end of the shrunk database")
	}
	want := []string{"checkpointed", "in-wal-a"}
	if got := rowsInTbl1(t, db); !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %q, want %q", got, want)
	}
}

// An empty -wal file is what a freshly checkpointed database leaves behind.
func TestWALEmptyIgnored(t *testing.T) {
	path, cleanup := copyDB(t, false)
	defer cleanup()
	if err := ioutil.WriteFile(path+"-wal", nil, 0644); err != nil {
		t.Fatal(err)
	}

	assertRows(t, path, walStaleRows)
}
