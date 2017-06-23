// Copyright 2017 The File Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package file implements a mechanism to allocate and deallocate parts of an
// os.File-like entity.
package file

import (
	"fmt"
	"io"
	"os"
	"unsafe"

	"github.com/cznic/internal/buffer"
	"github.com/cznic/mathutil"
)

const (
	allocAllign   = 16
	bufSize       = 1 << 20 // Calloc, Realloc.
	firstPageRank = maxSharedRank + 1
	maxSharedRank = slotRanks - 1
	maxSlot       = 1024
	oFilePages    = int64(unsafe.Offsetof(file{}.pages))
	oFileSkip     = int64(unsafe.Offsetof(file{}.skip))
	oFileSlots    = int64(unsafe.Offsetof(file{}.slots))
	oNodeNext     = int64(unsafe.Offsetof(node{}.next))
	oNodePrev     = int64(unsafe.Offsetof(node{}.prev))
	oPageBrk      = int64(unsafe.Offsetof(page{}.brk))
	oPageNext     = int64(unsafe.Offsetof(page{}.next))
	oPagePrev     = int64(unsafe.Offsetof(page{}.prev))
	oPageRank     = int64(unsafe.Offsetof(page{}.rank))
	oPageSize     = int64(unsafe.Offsetof(page{}.size))
	oPageUsed     = int64(unsafe.Offsetof(page{}.used))
	pageAvail     = pageSize - szPage - szTail
	pageLog       = 12
	pageMask      = pageSize - 1
	pageSize      = 1 << pageLog
	ranks         = 23
	slotRanks     = 7
	szFile        = int64(unsafe.Sizeof(file{}))
	szNode        = int64(unsafe.Sizeof(node{}))
	szPage        = int64(unsafe.Sizeof(page{}))
	szTail        = int64(unsafe.Sizeof(int64(0)))
)

var _ File = (*os.File)(nil)

func init() {
	if szFile%allocAllign != 0 || szFile != 256 || szNode%allocAllign != 0 || szPage%allocAllign != 0 {
		panic("internal error: invalid configuration")
	}
}

// 0:     1 -      1
// 1:    10 -     10
// 2:    11 -    100
// 3:   101 -   1000
// 4:  1001 -  10000
// 5: 10001 - 100000
// ...
//
// 1<<log(n) is the rounded up to nearest power of 2 storage size required for
// n bytes.
func log(n int) int {
	if n <= 0 {
		panic(fmt.Errorf("internal error: log(%v)", n))
	}

	return mathutil.BitLen(n - 1)
}

//  7:       1025 - 1*4096
//  8:   1*4096+1 - 2*4096
//  9:   2*4096+1 - 3*4096
//  ...
func pageRank(n int64) int {
	if n <= maxSlot {
		panic(fmt.Errorf("internal error: pageRank(%v)", n))
	}

	r := int(roundup64(n, pageSize)>>pageLog) + 6
	if r >= ranks {
		r = ranks - 1
	}
	return r
}

func read(b []byte) int64 {
	var n int64
	for _, v := range b[:8] {
		n = n<<8 | int64(v)
	}
	return n
}

func rank(n int64) int {
	if n <= maxSlot {
		return slotRank(int(n))
	}

	return pageRank(n)
}

func roundup(n, m int) int       { return (n + m - 1) &^ (m - 1) }
func roundup64(n, m int64) int64 { return (n + m - 1) &^ (m - 1) }

//  0:      1 -   16
//  1:     17 -   32
//  2:     33 -   64
//  3:     65 -  128
//  4:    129 -  256
//  5:    257 -  512
//  6:    513 - 1024
func slotRank(n int) int {
	if n < 1 || n > 1024 {
		panic(fmt.Errorf("internal error: slotRank(%v)", n))
	}

	return log(roundup(n, allocAllign)) - 4
}

func write(b []byte, n int64) {
	b = b[:8]
	for i := range b {
		b[i] = byte(n >> 56)
		n <<= 8
	}
}

type node struct {
	prev, next int64
}

type memNode struct {
	*Allocator
	dirty bool
	node
	off int64
}

func (m *memNode) flush() error {
	if !m.dirty {
		return nil
	}

	p := buffer.Get(int(szNode))
	b := *p
	write(b[oNodeNext:], m.next)
	write(b[oNodePrev:], m.prev)
	_, err := m.f.WriteAt(b, m.off)
	m.dirty = err == nil
	buffer.Put(p)
	return err
}

func (m *memNode) setNext(n int64) { m.next = n; m.dirty = true }
func (m *memNode) setPrev(n int64) { m.prev = n; m.dirty = true }

func (m *memNode) unlink(rank int) error {
	if m.prev != 0 {
		prev, err := m.openNode(m.prev)
		if err != nil {
			return err
		}

		prev.setNext(m.next)
		if err := prev.flush(); err != nil {
			return err
		}
	}

	if m.next != 0 {
		next, err := m.openNode(m.next)
		if err != nil {
			return err
		}

		next.setPrev(m.prev)
		if err := next.flush(); err != nil {
			return err
		}
	}

	if m.slots[rank] == m.off {
		m.setSlot(rank, m.next)
	}

	return nil
}

type page struct {
	brk int64
	node
	rank int64
	size int64
	used int64
}

type memPage struct {
	*Allocator
	dirty bool
	off   int64
	page
}

func (m *memPage) flush() error {
	if !m.dirty {
		return nil
	}

	p := buffer.Get(int(szPage))
	b := *p
	write(b[oPageBrk:], m.brk)
	write(b[oPageNext:], m.next)
	write(b[oPagePrev:], m.prev)
	write(b[oPageRank:], m.rank)
	write(b[oPageSize:], m.size)
	write(b[oPageUsed:], m.used)
	_, err := m.f.WriteAt(b, m.off)
	m.dirty = err == nil
	buffer.Put(p)
	return err
}

func (m *memPage) freeSlots() error {
	if m.used != 0 {
		return fmt.Errorf("internal error: %T.freeSlots: m.used %v", m, m.used)
	}

	for i := 0; i < int(m.brk); i++ {
		n, err := m.openNode(m.slot(i))
		if err != nil {
			return err
		}

		if err := n.unlink(int(m.rank)); err != nil {
			return err
		}

		if err := n.flush(); err != nil {
			return err
		}
	}
	return nil
}

func (m *memPage) setBrk(n int64)  { m.brk = n; m.dirty = true }
func (m *memPage) setNext(n int64) { m.next = n; m.dirty = true }
func (m *memPage) setPrev(n int64) { m.prev = n; m.dirty = true }
func (m *memPage) setRank(n int64) { m.rank = n; m.dirty = true }
func (m *memPage) setSize(n int64) { m.size = n; m.dirty = true }

func (m *memPage) setTail(n int64) error {
	p := buffer.Get(8)
	b := *p
	write(b, n)
	_, err := m.f.WriteAt(b, m.off+m.size-szTail)
	buffer.Put(p)
	return err
}

func (m *memPage) setUsed(n int64)  { m.used = n; m.dirty = true }
func (m *memPage) slot(i int) int64 { return m.off + szPage + int64(i)<<uint(m.rank+4) }

func (m *memPage) split(need int64) (int64, error) {
	if m.rank <= maxSharedRank {
		return -1, fmt.Errorf("internal error: %T.split: m.rank %v", m.rank)
	}

	have := m.size
	m.setSize(need)
	m.setRank(int64(pageRank(m.size)))
	if err := m.flush(); err != nil {
		return -1, err
	}

	if err := m.setTail(0); err != nil {
		return -1, err
	}

	n := m.newMemPage(m.off + m.size)
	n.setSize(have - need)
	n.setRank(int64(pageRank(have - need)))
	m.npages++
	if err := m.insertPage(n); err != nil {
		return -1, err
	}

	if err := n.flush(); err != nil {
		return -1, err
	}

	if err := n.setTail(n.size); err != nil {
		return -1, err
	}

	return m.off + szPage, m.Allocator.flush()
}

func (m *memPage) unlink() error {
	if m.prev != 0 {
		prev, err := m.openPage(m.prev)
		if err != nil {
			return err
		}

		prev.setNext(m.next)
		if err := prev.flush(); err != nil {
			return err
		}
	}

	if m.next != 0 {
		next, err := m.openPage(m.next)
		if err != nil {
			return err
		}

		next.setPrev(m.prev)
		if err := next.flush(); err != nil {
			return err
		}
	}

	if m.pages[m.rank] == m.off {
		m.setPage(int(m.rank), m.next)
	}

	m.setPrev(0)
	m.setNext(0)
	return nil
}

// File is an os.File-like entity.
type File interface {
	Close() error
	Stat() (os.FileInfo, error)
	Sync() error
	Truncate(int64) error
	io.ReaderAt
	io.WriterAt
}

type file struct {
	_ [16]byte // User area. Magic file number etc.

	// Persistent part.
	skip  [0]byte
	pages [ranks]int64
	slots [slotRanks]int64
}

type testStat struct {
	allocs int64
	bytes  int64
	npages int64
}

// Allocator manages allocation of file blocks within a File.
type Allocator struct {
	buf   []byte
	bufp  *[]byte
	cap   [slotRanks]int
	dirty bool
	f     File
	file
	fsize int64
	testStat
}

// NewAllocator returns a newly created Allocator managing f or an eror, if
// any. Allocator never touches the first 16 bytes within f.
func NewAllocator(f File) (*Allocator, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	a := &Allocator{
		bufp:  buffer.CGet(int(szFile - oFileSkip)),
		f:     f,
		fsize: fi.Size(),
	}
	a.buf = *a.bufp
	for i := range a.cap {
		a.cap[i] = int(pageAvail) / (1 << uint(i+4))
	}

	switch {
	case a.fsize <= oFileSkip:
		if _, err := f.WriteAt(a.buf, oFileSkip); err != nil {
			return nil, err
		}
	default:
		if n, err := f.ReadAt(a.buf, oFileSkip); n != len(a.buf) {
			return nil, err
		}

		max := a.fsize - szPage
		for i := range a.pages {
			if a.pages[i], err = a.check(read(a.buf[int(oFilePages-oFileSkip)+8*i:]), 0, max); err != nil {
				return nil, err
			}
		}
		for i := range a.slots {
			if a.slots[i], err = a.check(read(a.buf[int(oFileSlots-oFileSkip)+8*i:]), 0, max); err != nil {
				return nil, err
			}
		}
	}
	return a, nil
}

// Alloc allocates a file block large enough for storing size bytes and returns
// its offset or an error, if any.
func (a *Allocator) Alloc(size int64) (int64, error) {
	if size <= 0 {
		return -1, fmt.Errorf("invalid argument: %T.Alloc(%v)", a, size)
	}

	a.allocs++
	if size > maxSlot {
		return a.allocBig(size)
	}

	rank := slotRank(int(size))
	if off := a.pages[rank]; off != 0 {
		return a.sbrk(off, rank)
	}

	if off := a.pages[firstPageRank]; off != 0 {
		return a.sbrk2(off, rank)
	}

	if off := a.slots[rank]; off != 0 {
		return a.allocSlot(off, rank)
	}

	p, err := a.newSharedPage(rank)
	if err != nil {
		return -1, err
	}

	if err := a.insertPage(p); err != nil {
		return -1, err
	}

	p.setUsed(1)
	p.setBrk(1)
	if err := p.flush(); err != nil {
		return -1, err
	}

	return p.slot(0), a.flush()
}

// Calloc is like Alloc but the allocated file block is zeroed up to size.
func (a *Allocator) Calloc(size int64) (int64, error) {
	off, err := a.Alloc(size)
	if err != nil {
		return -1, err
	}

	p := buffer.CGet(int(mathutil.MinInt64(bufSize, size)))
	b := *p
	dst := off
	for size != 0 {
		rq := len(b)
		if size < int64(rq) {
			rq = int(size)
		}
		if _, err := a.f.WriteAt(b[:rq], dst); err != nil {
			return -1, err
		}

		dst += int64(rq)
		size -= int64(rq)
	}

	buffer.Put(p)
	return off, nil
}

// Close closes a and its underlying File.
func (a *Allocator) Close() error {
	if err := a.flush(); err != nil {
		return err
	}

	buffer.Put(a.bufp)
	return a.f.Close()
}

// Free recycles the allocated file block at off.
func (a *Allocator) Free(off int64) error {
	if off < szFile+szPage {
		return fmt.Errorf("invalid argument: %T.Free(%v)", a, off)
	}

	a.allocs--
	p, err := a.openPage((off-szFile)&^pageMask + szFile)
	if err != nil {
		return err
	}

	if p.rank > maxSharedRank {
		if err := a.freePage(p); err != nil {
			return err
		}

		return a.flush()
	}

	p.setUsed(p.used - 1)
	if err := a.insertSlot(int(p.rank), off); err != nil {
		return err
	}

	if p.used == 0 {
		if err := a.freePage(p); err != nil {
			return err
		}

		return a.flush()
	}

	if err := p.flush(); err != nil {
		return err
	}

	return a.flush()
}

// Realloc changes the size of the file block allocated at off, which must have
// been returned from Alloc or Realloc, to size and returns the offset of the
// relocated file block or an error, if any. The contents will be unchanged in
// the range from the start of the region up to the minimum of the old and new
// sizes. Realloc(off, 0) is equal to Free(off). If the file block was moved, a
// Free(off) is done.
func (a *Allocator) Realloc(off, size int64) (int64, error) {
	if off < szFile+szPage {
		return -1, fmt.Errorf("invalid argument: %T.Realloc(%v)", a, off)
	}

	if size == 0 {
		return -1, a.Free(off)
	}

	oldSize, p, err := a.usableSize(off)
	if err != nil {
		return -1, err
	}

	if oldSize >= size {
		newRank := rank(size)
		if int(p.rank) == newRank {
			return off, nil
		}

		if newRank > maxSharedRank {
			if need := roundup64(szPage+size+szTail, pageSize); p.size > need {
				return p.split(need)
			}
		}
	}

	newOff, err := a.Alloc(size)
	if err != nil {
		return -1, err
	}

	rem := mathutil.MinInt64(oldSize, size)
	q := buffer.Get(int(mathutil.MinInt64(bufSize, rem)))
	b := *q
	src := off
	dst := newOff
	for rem != 0 {
		n, err := a.f.ReadAt(b, src)
		if n == 0 {
			return -1, err
		}

		if _, err := a.f.WriteAt(b[:n], dst); err != nil {
			return -1, err
		}

		src += int64(n)
		dst += int64(n)
		rem -= int64(n)
	}
	buffer.Put(q)
	return newOff, a.Free(off)
}

// UsableSize reports the size of the file block allocated at off, which must
// have been returned from Alloc or Realloc.  The allocated file block size can
// be larger than the size originally requested from Alloc or Realloc.
func (a *Allocator) UsableSize(off int64) (int64, error) {
	n, _, err := a.usableSize(off)
	return n, err
}

func (a *Allocator) allocBig(size int64) (int64, error) {
	need := roundup64(szPage+size+szTail, pageSize)
	rank := pageRank(need)
	for i := rank; i < len(a.pages); i++ {
		off := a.pages[i]
		if off == 0 {
			continue
		}

		if i < ranks-1 {
			return a.allocBig2(off)
		}

		for j := 0; off != 0 && j < 2; j++ {
			p, err := a.openPage(off)
			if err != nil {
				return -1, err
			}

			if p.size >= need {
				return a.allocMaxRank(p, need)
			}

			off = p.next
		}
	}

	p, err := a.newPage(size)
	if err != nil {
		return -1, err
	}

	if err := p.flush(); err != nil {
		return -1, err
	}

	return p.off + szPage, a.flush()
}

func (a *Allocator) allocBig2(off int64) (int64, error) {
	p, err := a.openPage(off)
	if err != nil {
		return -1, err
	}

	if err := p.unlink(); err != nil {
		return -1, err
	}

	if err := p.flush(); err != nil {
		return -1, err
	}

	if err := p.setTail(0); err != nil {
		return -1, err
	}

	return p.off + szPage, a.flush()
}

func (a *Allocator) allocMaxRank(p *memPage, need int64) (int64, error) {
	if err := p.unlink(); err != nil {
		return -1, err
	}

	rem := p.size - need
	p.setSize(need)
	p.setRank(int64(pageRank(p.size)))
	if err := p.flush(); err != nil {
		return -1, err
	}

	if err := p.setTail(0); err != nil {
		return -1, err
	}

	if rem != 0 {
		q := a.newMemPage(p.off + p.size)
		q.setSize(rem)
		q.setRank(int64(pageRank(rem)))
		a.npages++
		if err := a.insertPage(q); err != nil {
			return -1, err
		}

		if err := q.flush(); err != nil {
			return -1, err
		}

		if err := q.setTail(q.size); err != nil {
			return -1, err
		}
	}

	return p.off + szPage, a.flush()
}

func (a *Allocator) allocSlot(off int64, rank int) (int64, error) {
	n, err := a.openNode(off)
	if err != nil {
		return -1, err
	}

	if err := n.unlink(rank); err != nil {
		return -1, err
	}

	p, err := a.openPage((off-szFile)&^pageMask + szFile)
	if err != nil {
		return -1, err
	}

	p.setUsed(p.used + 1)
	if err := p.flush(); err != nil {
		return -1, err
	}

	return off, a.flush()
}

func (a *Allocator) check(n, min, max int64) (int64, error) {
	if n < min || n > max {
		return 0, fmt.Errorf("corrupted file")
	}

	return n, nil
}

func (a *Allocator) flush() error {
	if !a.dirty {
		return nil
	}

	for i, v := range a.pages {
		write(a.buf[int(oFilePages-oFileSkip)+8*i:], v)
	}
	for i, v := range a.slots {
		write(a.buf[int(oFileSlots-oFileSkip)+8*i:], v)
	}
	_, err := a.f.WriteAt(a.buf, oFileSkip)
	a.dirty = err == nil
	return err
}

func (a *Allocator) freeLastPage(p *memPage) error {
	for {
		if p.rank <= maxSharedRank {
			if err := p.freeSlots(); err != nil {
				return err
			}
		}
		if err := p.unlink(); err != nil {
			return err
		}

		if err := p.flush(); err != nil {
			return err
		}

		if err := a.f.Truncate(p.off); err != nil {
			return err
		}

		a.fsize = p.off
		a.npages--
		a.bytes -= p.size
		if p.off > szFile {
			prevSize, err := a.read(p.off - szTail)
			if err != nil {
				return err
			}

			if prevSize != 0 {
				if p, err = a.openPage(p.off - prevSize); err != nil {
					return err
				}

				continue
			}
		}
		return nil
	}
}

func (a *Allocator) freePage(p *memPage) error {
	if p.used != 0 {
		return fmt.Errorf("internal error: %T.freePage: p.used %v", a, p.used)
	}

	if p.off+p.size == a.fsize {
		return a.freeLastPage(p)
	}

	if p.rank <= maxSharedRank {
		if err := p.freeSlots(); err != nil {
			return err
		}

		if err := p.unlink(); err != nil {
			return err
		}

		p.setBrk(0)
		p.setRank(firstPageRank)
	}
	if err := a.insertPage(p); err != nil {
		return err
	}

	if err := p.flush(); err != nil {
		return err
	}

	return p.setTail(p.size)
}

func (a *Allocator) insertPage(p *memPage) error {
	if p.prev != 0 || p.next != 0 {
		panic(fmt.Errorf("internal error: %T insertPage: p.prev %#x, p.next %#x", a, p.prev, p.next))
	}

	p.setNext(a.pages[p.rank])
	if p.next != 0 {
		next, err := a.openPage(p.next)
		if err != nil {
			return err
		}

		next.setPrev(p.off)
		if err := next.flush(); err != nil {
			return err
		}
	}
	a.setPage(int(p.rank), p.off)
	return nil
}

func (a *Allocator) insertSlot(rank int, off int64) error {
	m := memNode{Allocator: a, off: off}
	m.setNext(a.slots[rank])
	if m.next != 0 {
		next, err := a.openNode(m.next)
		if err != nil {
			return err
		}

		next.setPrev(off)
		if err := next.flush(); err != nil {
			return err
		}
	}
	a.setSlot(rank, off)
	return m.flush()
}

func (a *Allocator) newMemPage(off int64) *memPage { return &memPage{Allocator: a, off: off} }

func (a *Allocator) newPage(size int64) (*memPage, error) {
	off := roundup64(a.fsize-szFile, pageSize) + szFile
	size = roundup64(szPage+size+szTail, pageSize)
	p := a.newMemPage(off)
	p.setRank(int64(pageRank(size)))
	p.setSize(size)
	a.bytes += size
	a.fsize = off + size
	a.npages++
	return p, p.setTail(0)
}

func (a *Allocator) newSharedPage(rank int) (*memPage, error) {
	off := roundup64(a.fsize-szFile, pageSize) + szFile
	p := a.newMemPage(off)
	p.setRank(int64(rank))
	p.setSize(pageSize)
	a.bytes += pageSize
	a.fsize = off + pageSize
	a.npages++
	return p, p.setTail(0)
}

func (a *Allocator) openNode(off int64) (*memNode, error) {
	p := buffer.Get(int(szNode))
	b := *p
	if n, err := a.f.ReadAt(b, off); n != len(b) {
		return nil, err
	}

	m := &memNode{
		Allocator: a,
		off:       off,
		node: node{
			next: read(b[oNodeNext:]),
			prev: read(b[oNodePrev:]),
		},
	}
	buffer.Put(p)
	return m, nil
}

func (a *Allocator) openPage(off int64) (*memPage, error) {
	p := buffer.Get(int(szPage))
	b := *p
	if n, err := a.f.ReadAt(b, off); n != len(b) {
		return nil, err
	}

	m := &memPage{
		Allocator: a,
		off:       off,
		page: page{
			brk: read(b[oPageBrk:]),
			node: node{
				next: read(b[oPageNext:]),
				prev: read(b[oPagePrev:]),
			},
			rank: read(b[oPageRank:]),
			size: read(b[oPageSize:]),
			used: read(b[oPageUsed:]),
		},
	}
	buffer.Put(p)
	return m, nil
}

func (a *Allocator) read(off int64) (int64, error) {
	p := buffer.Get(8)
	b := *p
	if n, err := a.f.ReadAt(b, off); n != len(b) {
		return -1, err
	}

	n := read(b)
	buffer.Put(p)
	return n, nil
}

func (a *Allocator) sbrk(off int64, rank int) (int64, error) {
	p, err := a.openPage(off)
	if err != nil {
		return -1, err
	}

	if int64(rank) != p.rank {
		panic(fmt.Errorf("internal error: %T.sbrk: rank %v, p.rank %v", a, rank, p.rank))
	}

	p.setUsed(p.used + 1)
	p.setBrk(p.brk + 1)
	if int(p.brk) == a.cap[rank] {
		if err := p.unlink(); err != nil {
			return -1, err
		}
	}
	if err := p.flush(); err != nil {
		return -1, err
	}

	return p.slot(int(p.brk) - 1), a.flush()
}

func (a *Allocator) sbrk2(off int64, rank int) (int64, error) {
	p, err := a.openPage(off)
	if err != nil {
		return -1, err
	}

	if err := p.unlink(); err != nil {
		return -1, err
	}

	p.setRank(int64(rank))
	p.setUsed(1)
	p.setBrk(1)
	if err := a.insertPage(p); err != nil {
		return -1, err
	}

	if err := p.flush(); err != nil {
		return -1, err
	}

	if err := p.setTail(0); err != nil {
		return -1, err
	}

	return p.off + szPage, a.flush()
}

func (a *Allocator) setPage(rank int, n int64) { a.pages[rank] = n; a.dirty = true }
func (a *Allocator) setSlot(rank int, n int64) { a.slots[rank] = n; a.dirty = true }

func (a *Allocator) usableSize(off int64) (int64, *memPage, error) {
	if off < szFile+szPage {
		return -1, nil, fmt.Errorf("invalid argument: %T.UsableSize(%v)", a, off)
	}

	p, err := a.openPage((off-szFile)&^pageMask + szFile)
	if err != nil {
		return -1, nil, err
	}

	if p.rank < 0 || p.rank >= ranks {
		panic(fmt.Errorf("internal error: %T.UsableSize: p.rank %v", a, p.rank))
	}

	if p.rank <= maxSharedRank {
		return int64(1 << uint(p.rank+4)), p, nil
	}

	return p.size - szPage - szTail, p, nil
}