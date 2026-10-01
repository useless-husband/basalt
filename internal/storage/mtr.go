package storage

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/useless-husband/basalt/internal/pgerr"
)

// Mtr is a mini-transaction: a group of page changes that reach the WAL as
// one record and therefore survive a crash all together or not at all. A
// B+tree split, which touches several pages, is one mini-transaction.
//
// Pages are latched exclusively when first touched and stay latched until
// Commit or Abort. A mini-transaction also holds the checkpoint lock in
// shared mode, so a checkpoint never sees a half-applied change.
//
// Rule: a goroutine must not wait for row locks or other transactions while
// a mini-transaction is open, and must not open two at once.
type Mtr struct {
	s     *Store
	pages []*mtrPage
	done  bool
}

type mtrPage struct {
	f      *Frame
	before []byte
	isNew  bool
}

// pagePool recycles the before-image buffers of mini-transactions.
var pagePool = sync.Pool{New: func() any { b := make([]byte, PageSize); return &b }}

func pageCopy(src []byte) []byte {
	b := *(pagePool.Get().(*[]byte))
	copy(b, src)
	return b
}

// Begin starts a mini-transaction.
func (s *Store) Begin() *Mtr {
	s.ckpt.RLock()
	return &Mtr{s: s}
}

func (m *Mtr) find(id PageID) *mtrPage {
	for _, p := range m.pages {
		if p.f.id == id {
			return p
		}
	}
	return nil
}

// Page returns page id latched for writing.
func (m *Mtr) Page(id PageID) ([]byte, error) {
	if p := m.find(id); p != nil {
		return p.f.Data, nil
	}
	f, err := m.s.pool.fetch(id, true)
	if err != nil {
		return nil, err
	}
	f.Latch.Lock()
	m.pages = append(m.pages, &mtrPage{f: f, before: pageCopy(f.Data)})
	return f.Data, nil
}

// fresh latches a page whose old content is irrelevant (newly allocated).
func (m *Mtr) fresh(id PageID) ([]byte, error) {
	if p := m.find(id); p != nil {
		clear(p.f.Data)
		p.isNew = true
		return p.f.Data, nil
	}
	f, err := m.s.pool.fetch(id, false)
	if err != nil {
		return nil, err
	}
	f.Latch.Lock()
	before := pageCopy(f.Data)
	clear(f.Data)
	m.pages = append(m.pages, &mtrPage{f: f, before: before, isNew: true})
	return f.Data, nil
}

func (m *Mtr) release() {
	for _, p := range m.pages {
		p.f.Latch.Unlock()
		m.s.pool.unpin(p.f)
		b := p.before
		p.before = nil
		pagePool.Put(&b)
	}
	m.pages = nil
	if !m.done {
		m.done = true
		m.s.ckpt.RUnlock()
	}
}

// Abort undoes every change made through the mini-transaction.
func (m *Mtr) Abort() {
	if m.done {
		return
	}
	for _, p := range m.pages {
		copy(p.f.Data, p.before)
	}
	m.release()
}

// Commit logs the changes and releases the pages. It returns the LSN of
// the end of the record (0 if nothing changed).
func (m *Mtr) Commit() (LSN, error) {
	if m.done {
		return 0, pgerr.Internal("mini-transaction already finished")
	}
	rec := make([]byte, 0, 256)
	var changed []*mtrPage
	for _, p := range m.pages {
		if !p.isNew && bytes.Equal(p.before, p.f.Data) {
			continue
		}
		changed = append(changed, p)
		fpi := p.isNew || pageLSN(p.before) <= m.s.redoLSN
		rec = binary.LittleEndian.AppendUint32(rec, uint32(p.f.id))
		if fpi {
			rec = append(rec, 0)
			rec = append(rec, p.f.Data...)
		} else {
			rec = append(rec, 1)
			rec = appendDiff(rec, p.before, p.f.Data)
		}
	}
	if len(changed) == 0 {
		m.release()
		return 0, nil
	}
	lsn, err := m.s.wal.Append(RecPages, rec)
	if err != nil {
		m.Abort()
		return 0, err
	}
	for _, p := range changed {
		setPageLSN(p.f.Data, lsn)
		p.f.dirty.Store(true)
	}
	m.s.walSinceCkpt.Add(int64(len(rec)))
	m.release()
	return lsn, nil
}

// appendDiff encodes the byte ranges that differ between two page images:
// count u16, then (offset u16, length u16, bytes) per range. Ranges closer
// than 8 bytes are merged.
func appendDiff(dst, before, after []byte) []byte {
	type rng struct{ off, end int }
	var rs []rng
	i := pageHeader
	for i < PageSize {
		if before[i] == after[i] {
			// Skip equal 8-byte words quickly.
			if i%8 == 0 && i+8 <= PageSize && binary.LittleEndian.Uint64(before[i:]) == binary.LittleEndian.Uint64(after[i:]) {
				i += 8
				continue
			}
			i++
			continue
		}
		start := i
		end := i + 1
		for j := i + 1; j < PageSize && j < end+8; j++ {
			if before[j] != after[j] {
				end = j + 1
			}
		}
		if len(rs) > 0 && start-rs[len(rs)-1].end < 8 {
			rs[len(rs)-1].end = end
		} else {
			rs = append(rs, rng{start, end})
		}
		i = end
	}
	// Also log a change of the page type byte (header area).
	if before[offType] != after[offType] {
		rs = append([]rng{{offType, offType + 1}}, rs...)
	}
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(rs)))
	for _, r := range rs {
		dst = binary.LittleEndian.AppendUint16(dst, uint16(r.off))
		dst = binary.LittleEndian.AppendUint16(dst, uint16(r.end-r.off))
		dst = append(dst, after[r.off:r.end]...)
	}
	return dst
}

// applyPagesRecord replays a RecPages record (recovery).
func (s *Store) applyPagesRecord(r walRecord) error {
	b := r.payload
	for len(b) > 0 {
		if len(b) < 5 {
			return fmt.Errorf("wal record at %d: truncated page entry", r.lsn)
		}
		id := PageID(binary.LittleEndian.Uint32(b))
		kind := b[4]
		b = b[5:]
		switch kind {
		case 0: // full page image
			if len(b) < PageSize {
				return fmt.Errorf("wal record at %d: truncated page image", r.lsn)
			}
			f, err := s.pool.fetch(id, false)
			if err != nil {
				return err
			}
			copy(f.Data, b[:PageSize])
			setPageLSN(f.Data, r.end)
			f.dirty.Store(true)
			s.pool.unpin(f)
			b = b[PageSize:]
			if id >= s.recoveredPages {
				s.recoveredPages = id + 1
			}
		case 1: // diff
			if len(b) < 2 {
				return fmt.Errorf("wal record at %d: truncated diff", r.lsn)
			}
			n := int(binary.LittleEndian.Uint16(b))
			b = b[2:]
			f, err := s.pool.fetch(id, true)
			if err != nil {
				return fmt.Errorf("recovery of page %d at lsn %d: %w", id, r.lsn, err)
			}
			apply := pageLSN(f.Data) < r.end
			for k := 0; k < n; k++ {
				if len(b) < 4 {
					s.pool.unpin(f)
					return fmt.Errorf("wal record at %d: truncated diff range", r.lsn)
				}
				off := int(binary.LittleEndian.Uint16(b))
				l := int(binary.LittleEndian.Uint16(b[2:]))
				b = b[4:]
				if len(b) < l || off+l > PageSize {
					s.pool.unpin(f)
					return fmt.Errorf("wal record at %d: bad diff range", r.lsn)
				}
				if apply {
					copy(f.Data[off:], b[:l])
				}
				b = b[l:]
			}
			if apply {
				setPageLSN(f.Data, r.end)
				f.dirty.Store(true)
			}
			s.pool.unpin(f)
		default:
			return fmt.Errorf("wal record at %d: unknown page entry kind %d", r.lsn, kind)
		}
	}
	return nil
}
