package storage

import (
	"sync"

	"github.com/useless-husband/basalt/internal/pgerr"
)

// The commit log (clog) records the status of every transaction id with two
// bits: in progress (0), committed (1) or aborted (2). It lives in ordinary
// pages, so it is protected by the WAL like everything else; the list of
// clog pages is kept in the meta page.

// Transaction status values.
const (
	XidInProgress byte = 0
	XidCommitted  byte = 1
	XidAborted    byte = 2
)

const (
	clogBytesPerPage = PageSize - pageHeader
	// XidsPerClogPage is how many transaction statuses fit in a page.
	XidsPerClogPage = clogBytesPerPage * 4
)

type clogDir struct {
	mu    sync.Mutex
	pages []PageID
	ready bool
}

func (s *Store) loadClogDir() error {
	if s.clog.ready {
		return nil
	}
	return s.readMeta(func(m []byte) {
		n := int(u32(m, offMetaClogN))
		s.clog.pages = make([]PageID, n)
		for i := 0; i < n; i++ {
			s.clog.pages[i] = PageID(u32(m, offMetaClogDir+4*i))
		}
		s.clog.ready = true
	})
}

// clogPage returns the clog page holding xid, allocating pages as needed.
func (s *Store) clogPage(xid uint64) (PageID, error) {
	idx := int(xid / XidsPerClogPage)
	s.clog.mu.Lock()
	defer s.clog.mu.Unlock()
	if err := s.loadClogDir(); err != nil {
		return 0, err
	}
	for len(s.clog.pages) <= idx {
		if len(s.clog.pages) >= maxClogPages {
			return 0, pgerr.New(pgerr.ProgramLimitExceeded, "transaction id space exhausted (%d ids)", maxClogPages*XidsPerClogPage)
		}
		m := s.Begin()
		id, _, err := m.Alloc(PageClog)
		if err != nil {
			m.Abort()
			return 0, err
		}
		meta, err := m.Page(0)
		if err != nil {
			m.Abort()
			return 0, err
		}
		n := len(s.clog.pages)
		put32(meta, offMetaClogDir+4*n, uint32(id))
		put32(meta, offMetaClogN, uint32(n+1))
		if _, err := m.Commit(); err != nil {
			return 0, err
		}
		s.clog.pages = append(s.clog.pages, id)
	}
	return s.clog.pages[idx], nil
}

// SetXidStatus records a transaction's final status and returns the LSN of
// the change; a committer must flush the WAL up to it before reporting
// success.
func (s *Store) SetXidStatus(xid uint64, status byte) (LSN, error) {
	return s.SetXidStatusWith(xid, status, nil)
}

// SetXidStatusWith records a status like SetXidStatus, and applies more
// page changes (with) in the same mini-transaction, so that both reach the
// log as one atomic record. Transactional DDL uses it to publish the new
// catalog together with the commit.
func (s *Store) SetXidStatusWith(xid uint64, status byte, with func(m *Mtr) error) (LSN, error) {
	id, err := s.clogPage(xid)
	if err != nil {
		return 0, err
	}
	m := s.Begin()
	if with != nil {
		if err := with(m); err != nil {
			m.Abort()
			return 0, err
		}
	}
	p, err := m.Page(id)
	if err != nil {
		m.Abort()
		return 0, err
	}
	i := xid % XidsPerClogPage
	off := pageHeader + int(i/4)
	shift := (i % 4) * 2
	p[off] = p[off]&^(3<<shift) | status<<shift
	return m.Commit()
}

// ReadClog returns the raw status bytes of every clog page, concatenated
// (four statuses per byte, low bits first).
func (s *Store) ReadClog() ([]byte, error) {
	s.clog.mu.Lock()
	err := s.loadClogDir()
	pages := append([]PageID(nil), s.clog.pages...)
	s.clog.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(pages)*clogBytesPerPage)
	for _, id := range pages {
		f, err := s.pool.Read(id)
		if err != nil {
			return nil, err
		}
		out = append(out, f.Data[pageHeader:]...)
		s.pool.Release(f)
	}
	return out, nil
}
