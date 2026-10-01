// Package storage is basalt's storage engine: a single data file of
// fixed-size pages, a buffer pool with clock eviction, a write-ahead log
// with physiological redo records and full-page images, mini-transactions
// that make multi-page changes atomic, slotted heap pages, B+tree indexes,
// the transaction status log (clog) and crash recovery.
package storage

import (
	"encoding/binary"
	"hash/crc32"
)

// PageSize is the size of every page in the data file.
const PageSize = 8192

// PageID numbers pages from the start of the data file. Page 0 is the meta
// page; 0 is therefore also used as "no page" in page pointers.
type PageID uint32

// InvalidPage marks the absence of a page pointer.
const InvalidPage PageID = 0

// LSN is a log sequence number: a byte position in the write-ahead log.
type LSN uint64

// Page types.
const (
	PageFree  byte = 0
	PageMeta  byte = 1
	PageHeap  byte = 2
	PageBtree byte = 3
	PageClog  byte = 4
	PageBlob  byte = 5
	PageSeq   byte = 6
)

// Common page header, present on every page:
//
//	0..8   page LSN (end LSN of the last WAL record that changed the page)
//	8..12  CRC-32C of the page with this field zeroed
//	12     page type
//	13..16 reserved
const (
	offLSN      = 0
	offChecksum = 8
	offType     = 12
	pageHeader  = 16
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func pageLSN(p []byte) LSN       { return LSN(binary.LittleEndian.Uint64(p[offLSN:])) }
func setPageLSN(p []byte, l LSN) { binary.LittleEndian.PutUint64(p[offLSN:], uint64(l)) }
func pageType(p []byte) byte     { return p[offType] }

// pageChecksum computes the checksum of a page image.
func pageChecksum(p []byte) uint32 {
	c := crc32.Update(0, castagnoli, p[:offChecksum])
	c = crc32.Update(c, castagnoli, []byte{0, 0, 0, 0})
	return crc32.Update(c, castagnoli, p[offChecksum+4:])
}

func setChecksum(p []byte) {
	binary.LittleEndian.PutUint32(p[offChecksum:], pageChecksum(p))
}

// verifyChecksum accepts an all-zero page (allocated but never written) or
// a page whose checksum matches.
func verifyChecksum(p []byte) bool {
	if binary.LittleEndian.Uint32(p[offChecksum:]) == pageChecksum(p) {
		return true
	}
	for _, b := range p {
		if b != 0 {
			return false
		}
	}
	return true
}

func u16(p []byte, off int) uint16      { return binary.LittleEndian.Uint16(p[off:]) }
func put16(p []byte, off int, v uint16) { binary.LittleEndian.PutUint16(p[off:], v) }
func u32(p []byte, off int) uint32      { return binary.LittleEndian.Uint32(p[off:]) }
func put32(p []byte, off int, v uint32) { binary.LittleEndian.PutUint32(p[off:], v) }
func u64(p []byte, off int) uint64      { return binary.LittleEndian.Uint64(p[off:]) }
func put64(p []byte, off int, v uint64) { binary.LittleEndian.PutUint64(p[off:], v) }
