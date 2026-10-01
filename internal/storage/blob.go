package storage

import "github.com/useless-husband/basalt/internal/pgerr"

// Blobs are byte strings stored in a chain of pages (used for the
// serialized catalog). Layout after the common header: next page u32,
// length u16, data.
const (
	offBlobNext = pageHeader
	offBlobLen  = pageHeader + 4
	offBlobData = pageHeader + 8
	blobCap     = PageSize - offBlobData
)

// WriteBlob stores data in newly allocated pages and returns the first.
func (m *Mtr) WriteBlob(data []byte) (PageID, error) {
	var first PageID
	var prev []byte
	for len(data) > 0 || first == InvalidPage {
		id, p, err := m.Alloc(PageBlob)
		if err != nil {
			return 0, err
		}
		n := min(len(data), blobCap)
		copy(p[offBlobData:], data[:n])
		put16(p, offBlobLen, uint16(n))
		data = data[n:]
		if prev != nil {
			put32(prev, offBlobNext, uint32(id))
		} else {
			first = id
		}
		prev = p
	}
	return first, nil
}

// FreeBlob frees a blob chain.
func (m *Mtr) FreeBlob(first PageID) error {
	for id := first; id != InvalidPage; {
		p, err := m.Page(id)
		if err != nil {
			return err
		}
		if pageType(p) != PageBlob {
			return pgerr.New(pgerr.DataCorrupted, "page %d is not a blob page", id)
		}
		next := PageID(u32(p, offBlobNext))
		if err := m.Free(id); err != nil {
			return err
		}
		id = next
	}
	return nil
}

// ReadBlob reads a blob chain.
func (s *Store) ReadBlob(first PageID) ([]byte, error) {
	var out []byte
	for id := first; id != InvalidPage; {
		f, err := s.pool.Read(id)
		if err != nil {
			return nil, err
		}
		if pageType(f.Data) != PageBlob {
			s.pool.Release(f)
			return nil, pgerr.New(pgerr.DataCorrupted, "page %d is not a blob page", id)
		}
		n := int(u16(f.Data, offBlobLen))
		out = append(out, f.Data[offBlobData:offBlobData+n]...)
		id = PageID(u32(f.Data, offBlobNext))
		s.pool.Release(f)
	}
	return out, nil
}

// Sequence pages hold one 64-bit counter at offset pageHeader.

// AllocSeq allocates a sequence page with the given value.
func (m *Mtr) AllocSeq(v int64) (PageID, error) {
	id, p, err := m.Alloc(PageSeq)
	if err != nil {
		return 0, err
	}
	put64(p, pageHeader, uint64(v))
	return id, nil
}

// SetSeq stores a sequence high-water mark.
func (m *Mtr) SetSeq(id PageID, v int64) error {
	p, err := m.Page(id)
	if err != nil {
		return err
	}
	put64(p, pageHeader, uint64(v))
	return nil
}

// ReadSeq reads a sequence page.
func (s *Store) ReadSeq(id PageID) (int64, error) {
	f, err := s.pool.Read(id)
	if err != nil {
		return 0, err
	}
	defer s.pool.Release(f)
	return int64(u64(f.Data, pageHeader)), nil
}
