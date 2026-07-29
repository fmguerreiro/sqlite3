// Copyright 2017 The go-sqlite Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sqlite3

import (
	"bytes"
	"encoding/binary"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// SQLite writes a log's checksums in the byte order of the machine that created
// it, so a log from a big-endian host carries the other magic number and the
// other word order. There is no fixture from such a host to hand, so this makes
// one out of the little-endian fixture.
func TestWALBigEndianChecksums(t *testing.T) {
	path, cleanup := copyDB(t, true)
	defer cleanup()
	log, err := ioutil.ReadFile(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}

	binary.BigEndian.PutUint32(log[0:4], walMagicBigEndian)
	sealWAL(binary.BigEndian, log, 1024)
	if err := ioutil.WriteFile(path+"-wal", log, 0644); err != nil {
		t.Fatal(err)
	}

	assertRows(t, path, walRows)
}

// testdata/wal-shrink.sqlite: an auto-vacuuming transaction drops 60 rows and
// takes the database from 34 pages to 4. The shrink case for walIndex.dbSize.
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

// testdata/wal-grow.sqlite is 2 pages on disk; the logged transaction inserts
// 200 rows and commits at 5. The grow case for walIndex.dbSize, and the
// ordinary state of a database belonging to a running application.
func TestWALGrowsDatabase(t *testing.T) {
	db, err := Open("testdata/wal-grow.sqlite")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if got, want := db.NumPage(), 5; got != want {
		t.Errorf("NumPage() = %d, want %d", got, want)
	}
	// Page 5 lives only in the log; the main file stops after page 2.
	if _, err := db.pager.Page(5); err != nil {
		t.Errorf("Page(5): %v", err)
	}
	if got, want := len(rowsInTbl1(t, db)), 201; got != want {
		t.Errorf("len(rows) = %d, want %d", got, want)
	}
}

// A log restarted under a reader must fail the read, rather than serve a frame
// from the new generation as the page it used to be.
func TestWALDetectsRestartUnderReader(t *testing.T) {
	path, cleanup := copyDB(t, true)
	defer cleanup()

	index, err := openWAL(path, 1024)
	if err != nil {
		t.Fatalf("openWAL: %v", err)
	}
	if index == nil {
		t.Fatal("openWAL returned no index for the fixture log")
	}
	defer index.Close()

	buf := make([]byte, 1024)
	if ok, err := index.page(1, buf); !ok || err != nil {
		t.Fatalf("page(1) = %v, %v; want true, nil", ok, err)
	}

	// Rewrite the salt of the frame the index points at, as a restarted log
	// would. The index still holds the old salt.
	log, err := os.OpenFile(path+"-wal", os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.WriteAt([]byte{0, 0, 0, 0, 0, 0, 0, 0}, index.offsets[1]+8); err != nil {
		t.Fatal(err)
	}
	log.Close()

	if ok, err := index.page(1, buf); !ok || err != errWALChanged {
		t.Errorf("page(1) = %v, %v; want true, %v", ok, err, errWALChanged)
	}
}

// PRAGMA wal_checkpoint(TRUNCATE) restarts the log by emptying it, which leaves
// every indexed offset past the end rather than pointing at a wrong frame. A
// bare io.EOF here would read as an I/O fault instead of a restart.
func TestWALDetectsTruncationUnderReader(t *testing.T) {
	path, cleanup := copyDB(t, true)
	defer cleanup()

	index, err := openWAL(path, 1024)
	if err != nil {
		t.Fatalf("openWAL: %v", err)
	}
	if index == nil {
		t.Fatal("openWAL returned no index for the fixture log")
	}
	defer index.Close()

	if err := os.Truncate(path+"-wal", 0); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 1024)
	if ok, err := index.page(1, buf); !ok || err != errWALChanged {
		t.Errorf("page(1) = %v, %v; want true, %v", ok, err, errWALChanged)
	}
}

// The log's page 1 replaces the database header, so it has to clear the same
// bar the main file's page 1 did. A zeroed one used to reach the pager as a
// page size of zero and panic there; one with the wrong magic is not a
// database header at all.
func TestWALRejectsBadHeaderPage(t *testing.T) {
	real1, err := ioutil.ReadFile(walFixture)
	if err != nil {
		t.Fatal(err)
	}
	wrongMagic := append([]byte(nil), real1[:1024]...)
	wrongMagic[0] = 'X'

	tests := []struct {
		name  string
		page1 []byte
	}{
		// Zeroed is the panic that actually happened. Wrong magic keeps a
		// valid page size, so only the magic check can turn it away.
		{"zeroed", nil},
		{"wrong magic", wrongMagic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, cleanup := copyDB(t, false)
			defer cleanup()
			writeWAL(t, path, buildWAL(walMagicLittleEndian, walFormatVersion, 1024,
				[]walFrame{{pgno: 1, dbSize: 2, page: tt.page1}}))

			db, err := Open(path)
			if err == nil {
				db.Close()
				t.Error("Open accepted a log whose page 1 is not a database header")
			}
		})
	}
}

// A page 1 that is a valid header but disagrees with the main file about the
// page size cannot be applied to a pager already sized for the main file.
func TestWALRejectsHeaderPageResize(t *testing.T) {
	path, cleanup := copyDB(t, false)
	defer cleanup()
	main, err := ioutil.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// The real page 1, so the magic still passes, with only the page-size
	// field changed.
	page1 := append([]byte(nil), main[:1024]...)
	binary.BigEndian.PutUint16(page1[16:18], 512)
	writeWAL(t, path, buildWAL(walMagicLittleEndian, walFormatVersion, 1024,
		[]walFrame{{pgno: 1, dbSize: 2, page: page1}}))

	db, err := Open(path)
	if err == nil {
		db.Close()
		t.Fatal("Open accepted a log whose page 1 changes the page size")
	}
	if !strings.Contains(err.Error(), "changes the page size") {
		t.Errorf("Open: %v, want a page-size error", err)
	}
}

// sealWAL recomputes the checksum chain over log in place, so a test can edit a
// header field or a frame payload and still hand back a log that verifies. The
// chain is spelled out here rather than called through walChecksum, so that a
// bug in the latter cannot cancel itself out.
func sealWAL(order binary.ByteOrder, log []byte, pageSize int) {
	sum := func(s0, s1 uint32, b []byte) (uint32, uint32) {
		for i := 0; i+8 <= len(b); i += 8 {
			s0 += order.Uint32(b[i:i+4]) + s1
			s1 += order.Uint32(b[i+4:i+8]) + s0
		}
		return s0, s1
	}
	s0, s1 := sum(0, 0, log[0:24])
	binary.BigEndian.PutUint32(log[24:28], s0)
	binary.BigEndian.PutUint32(log[28:32], s1)
	frame := walFrameHeaderSize + pageSize
	for off := walHeaderSize; off+frame <= len(log); off += frame {
		f := log[off : off+frame]
		s0, s1 = sum(s0, s1, f[0:8])
		s0, s1 = sum(s0, s1, f[walFrameHeaderSize:])
		binary.BigEndian.PutUint32(f[16:20], s0)
		binary.BigEndian.PutUint32(f[20:24], s1)
	}
}

// walFrame is one frame to assemble into a synthetic log. A dbSize of zero
// makes it a non-commit frame; a nil page leaves the payload zeroed.
type walFrame struct {
	pgno   uint32
	dbSize uint32
	page   []byte
}

// buildWAL assembles a write-ahead log and seals it, so the result verifies as
// it stands. magic and version are parameters because rejecting the wrong ones
// is most of what these tests check; the word order follows the magic, as it
// does in a real log.
func buildWAL(magic, version uint32, pageSize int, frames []walFrame) []byte {
	order := binary.ByteOrder(binary.LittleEndian)
	if magic == walMagicBigEndian {
		order = binary.BigEndian
	}

	log := make([]byte, walHeaderSize)
	binary.BigEndian.PutUint32(log[0:4], magic)
	binary.BigEndian.PutUint32(log[4:8], version)
	binary.BigEndian.PutUint32(log[8:12], uint32(pageSize))
	copy(log[16:24], []byte("saltsalt"))

	for _, f := range frames {
		frame := make([]byte, walFrameHeaderSize+pageSize)
		binary.BigEndian.PutUint32(frame[0:4], f.pgno)
		binary.BigEndian.PutUint32(frame[4:8], f.dbSize)
		copy(frame[8:16], log[16:24])
		copy(frame[walFrameHeaderSize:], f.page)
		log = append(log, frame...)
	}
	sealWAL(order, log, pageSize)
	return log
}

// writeWAL lays log beside the database at path.
func writeWAL(t *testing.T, path string, log []byte) {
	t.Helper()
	if err := ioutil.WriteFile(path+"-wal", log, 0644); err != nil {
		t.Fatal(err)
	}
}

// The log overlays the main file rather than replacing it, so a page it does
// not carry still has to come from the main file. Every other fixture happens
// to log every page it commits, which never exercises the fall-through.
func TestWALFallsThroughToMainFile(t *testing.T) {
	path, cleanup := copyDB(t, false)
	defer cleanup()
	main, err := ioutil.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// A log carrying page 1 unchanged and committing at the main file's size.
	// The rows live on page 2, which only the main file has.
	writeWAL(t, path, buildWAL(walMagicLittleEndian, walFormatVersion, 1024,
		[]walFrame{{pgno: 1, dbSize: 2, page: main[:1024]}}))

	assertRows(t, path, walStaleRows)
}

// A checkpoint restarts the log with a fresh salt and writes over the old
// frames in place, so the scan has to stop at the first frame carrying a salt
// other than the header's rather than read into the previous generation.
func TestReadWALIndexStopsAtSaltChange(t *testing.T) {
	const pageSize = 1024
	log := buildWAL(walMagicLittleEndian, walFormatVersion, pageSize, []walFrame{
		{pgno: 1, dbSize: 1},
		{pgno: 2, dbSize: 2},
	})
	second := walHeaderSize + walFrameHeaderSize + pageSize
	copy(log[second+8:second+16], []byte("OTHERSLT"))
	sealWAL(binary.LittleEndian, log, pageSize)

	index, err := readWALIndex(bytes.NewReader(log), pageSize)
	if err != nil {
		t.Fatalf("readWALIndex: %v", err)
	}
	if index == nil {
		t.Fatal("readWALIndex dropped the frames before the salt change")
	}
	want := map[int]int64{1: walHeaderSize}
	if !reflect.DeepEqual(index.offsets, want) {
		t.Errorf("offsets = %v, want %v", index.offsets, want)
	}
	if index.dbSize != 1 {
		t.Errorf("dbSize = %d, want 1", index.dbSize)
	}
}

// Logs SQLite would refuse, or this package cannot apply. Each must read as
// "no snapshot here" rather than as an error, so the main file still opens.
func TestReadWALIndexRejects(t *testing.T) {
	const pageSize = 1024
	commit := []walFrame{{pgno: 1, dbSize: 1}}

	// Frames laid out at the size readWALIndex is called with, but a header
	// declaring another. Everything else about the log verifies, so only the
	// page-size check can turn it away.
	mislabelled := buildWAL(walMagicLittleEndian, walFormatVersion, pageSize, commit)
	binary.BigEndian.PutUint32(mislabelled[8:12], 512)
	sealWAL(binary.LittleEndian, mislabelled, pageSize)

	tests := []struct {
		name string
		log  []byte
	}{
		{"wrong magic", buildWAL(0xdeadbeef, walFormatVersion, pageSize, commit)},
		{"future format version", buildWAL(walMagicLittleEndian, walFormatVersion+1, pageSize, commit)},
		{"other page size", mislabelled},
		{"page zero", buildWAL(walMagicLittleEndian, walFormatVersion, pageSize, []walFrame{{pgno: 0, dbSize: 1}})},
		{"page count overflows int32", buildWAL(walMagicLittleEndian, walFormatVersion, pageSize, []walFrame{{pgno: 1, dbSize: 1 << 31}})},
		{"no commit frame", buildWAL(walMagicLittleEndian, walFormatVersion, pageSize, []walFrame{{pgno: 1}})},
		{"header only", buildWAL(walMagicLittleEndian, walFormatVersion, pageSize, nil)},
	}
	for _, tt := range tests {
		index, err := readWALIndex(bytes.NewReader(tt.log), pageSize)
		if err != nil {
			t.Errorf("%s: readWALIndex: %v", tt.name, err)
			continue
		}
		if index != nil {
			t.Errorf("%s: indexed %d pages, want no index", tt.name, len(index.offsets))
		}
	}
}

// The positive control for the table above, and for the rule that only frames
// up to the last commit frame count.
func TestReadWALIndexAppliesCommittedFrames(t *testing.T) {
	const pageSize = 1024
	log := buildWAL(walMagicLittleEndian, walFormatVersion, pageSize, []walFrame{
		{pgno: 1},
		{pgno: 2, dbSize: 2},
		{pgno: 3}, // after the commit, so uncommitted
	})

	index, err := readWALIndex(bytes.NewReader(log), pageSize)
	if err != nil {
		t.Fatalf("readWALIndex: %v", err)
	}
	if index == nil {
		t.Fatal("readWALIndex returned no index for a committed log")
	}
	frame := int64(walFrameHeaderSize + pageSize)
	want := map[int]int64{1: walHeaderSize, 2: walHeaderSize + frame}
	if !reflect.DeepEqual(index.offsets, want) {
		t.Errorf("offsets = %v, want %v", index.offsets, want)
	}
	if index.dbSize != 2 {
		t.Errorf("dbSize = %d, want 2", index.dbSize)
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
