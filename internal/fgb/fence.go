package fgb

import (
	"encoding/binary"
	"errors"
	"io"
)

var fenceMagic = [8]byte{'p', 'g', 'f', 'e', 'n', 'c', 'e', '1'}

const fenceHeaderBytes = 16
const FenceEvery = 256

type FenceEntry struct {
	Key    uint64
	ID     string
	Offset uint64
}

type Fence struct {
	entries []FenceEntry
}

func EncodeFence(entries []FenceEntry) []byte {
	size := fenceHeaderBytes
	for _, entry := range entries {
		size += 20 + len(entry.ID)
	}
	out := make([]byte, fenceHeaderBytes, size)
	copy(out, fenceMagic[:])
	binary.LittleEndian.PutUint64(out[8:], uint64(len(entries)))
	for _, entry := range entries {
		out = binary.LittleEndian.AppendUint64(out, entry.Key)
		out = binary.LittleEndian.AppendUint64(out, entry.Offset)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(entry.ID)))
		out = append(out, entry.ID...)
	}
	return out
}

func DecodeFence(data []byte) (*Fence, error) {
	if len(data) < fenceHeaderBytes {
		return nil, errors.New("fgb: truncated fence")
	}
	for i := range fenceMagic {
		if data[i] != fenceMagic[i] {
			return nil, errors.New("fgb: not a key fence")
		}
	}
	count := binary.LittleEndian.Uint64(data[8:])
	entries := make([]FenceEntry, 0, count)
	at := fenceHeaderBytes
	for i := uint64(0); i < count; i++ {
		if at+20 > len(data) {
			return nil, errors.New("fgb: truncated fence entries")
		}
		entry := FenceEntry{
			Key:    binary.LittleEndian.Uint64(data[at:]),
			Offset: binary.LittleEndian.Uint64(data[at+8:]),
		}
		length := int(binary.LittleEndian.Uint32(data[at+16:]))
		at += 20
		if length < 0 || at+length > len(data) {
			return nil, errors.New("fgb: truncated fence id")
		}
		entry.ID = string(data[at : at+length])
		at += length
		entries = append(entries, entry)
	}
	return &Fence{entries: entries}, nil
}

func ReadFence(r io.ReaderAt, size int64) (*Fence, error) {
	if size < fenceHeaderBytes {
		return nil, errors.New("fgb: truncated fence")
	}
	data := make([]byte, size)
	if _, err := r.ReadAt(data, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return DecodeFence(data)
}

func (f *Fence) Entries() []FenceEntry {
	if f == nil {
		return nil
	}
	return f.entries
}

func (f *Fence) Len() int {
	if f == nil {
		return 0
	}
	return len(f.entries)
}

func (f *Fence) StartRow(key uint64, id string) int {
	if f == nil || len(f.entries) == 0 {
		return 0
	}
	lo, hi := 0, len(f.entries)
	for lo < hi {
		mid := (lo + hi) / 2
		entry := f.entries[mid]
		if entry.Key < key || (entry.Key == key && entry.ID < id) {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo > 0 {
		lo--
	}
	if lo >= len(f.entries) {
		lo = len(f.entries) - 1
	}
	return lo * FenceEvery
}

func (f *Fence) RowOffset(row int) (uint64, bool) {
	if f == nil || row < 0 {
		return 0, false
	}
	at := row / FenceEvery
	if at >= len(f.entries) {
		return 0, false
	}
	return f.entries[at].Offset, true
}
