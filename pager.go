// Copyright 2017 The go-sqlite Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sqlite3

import (
	"fmt"
	"io"
)

type pager struct {
	f      io.ReadSeeker
	size   int          // page size in bytes
	npages int          // total number of pages in db
	pages  map[int]page // cache of pages
	lru    []int        // list of last used pages
	wal    *walIndex    // committed pages living in the write-ahead log, if any
}

func newPager(f io.ReadSeeker, size, npages int, wal *walIndex) pager {
	pager := pager{
		f:      f,
		size:   size,
		npages: npages,
		pages:  make(map[int]page, npages),
		lru:    make([]int, 0, 2),
		wal:    wal,
	}

	return pager
}

func (p *pager) Page(i int) (page, error) {
	page, ok := p.pages[i]
	if ok {
		return page, nil
	}

	if i > p.npages {
		return page, fmt.Errorf("sqlite3: out of range (%d > %d)", i, p.npages)
	}

	buf := make([]byte, p.size)
	if err := p.read(i, buf); err != nil {
		return page, err
	}

	page.id = i
	page.buf = buf

	p.pages[i] = page
	p.lru = append(p.lru, i)
	return page, nil
}

// read fills buf with page i, preferring the write-ahead log's image of it
// over the one in the main database file.
func (p *pager) read(i int, buf []byte) error {
	if p.wal != nil {
		if ok, err := p.wal.page(i, buf); ok || err != nil {
			return err
		}
	}

	pos, _ := p.f.Seek(0, io.SeekCurrent)
	defer p.f.Seek(pos, io.SeekStart)

	if _, err := p.f.Seek(int64((i-1)*p.size), io.SeekStart); err != nil {
		return err
	}
	n, err := p.f.Read(buf)
	if err != nil {
		return err
	}
	if n != len(buf) {
		return fmt.Errorf("sqlite3: read too few bytes")
	}
	return nil
}

func (p *pager) Delete() error {
	var err error
	p.pages = nil
	p.lru = nil
	if p.wal != nil {
		err = p.wal.Close()
		p.wal = nil
	}
	return err
}
