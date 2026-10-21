package dataset

import (
	"io"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

var parquetMagic = []byte("PAR1")

type VirtualFile struct {
	collection  string
	version     int64
	etag        string
	footer      []byte
	footerStart int64
	totalLength int64
	shards      []shardRecord
	pool        *pgxpool.Pool
	mu          sync.Mutex
	readers     map[int64]*chunkReader
}

func (v *VirtualFile) Size() int64        { return v.totalLength }
func (v *VirtualFile) ETag() string       { return v.etag }
func (v *VirtualFile) Version() int64     { return v.version }
func (v *VirtualFile) Collection() string { return v.collection }

func (v *VirtualFile) shardReader(shard shardRecord) *chunkReader {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.readers == nil {
		v.readers = make(map[int64]*chunkReader)
	}
	if r, ok := v.readers[shard.ID]; ok {
		return r
	}
	r := shardReader(nil, v.pool, shard.ID, shard.Bytes)
	v.readers[shard.ID] = r
	return r
}

func (v *VirtualFile) shardAt(off int64) (shardRecord, bool) {
	for _, shard := range v.shards {
		if off >= shard.VirtualBase && off < shard.VirtualBase+shardDataLength(shard) {
			return shard, true
		}
	}
	return shardRecord{}, false
}

func (v *VirtualFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, io.EOF
	}
	written := 0
	for written < len(p) {
		at := off + int64(written)
		if at >= v.totalLength {
			return written, io.EOF
		}
		switch {
		case at < 4:
			n := copy(p[written:], parquetMagic[at:])
			written += n
		case at < v.footerStart:
			shard, ok := v.shardAt(at)
			if !ok {
				return written, io.ErrUnexpectedEOF
			}
			base := shard.VirtualBase
			sourceOffset := shard.DataStart + (at - base)
			limit := shard.VirtualBase + shardDataLength(shard)
			n := int(min(int64(len(p)-written), limit-at))
			if n <= 0 {
				return written, io.ErrUnexpectedEOF
			}
			read, err := v.shardReader(shard).ReadAt(p[written:written+n], sourceOffset)
			written += read
			if err != nil && !(err == io.EOF && read == n) {
				return written, err
			}
		default:
			n := copy(p[written:], v.footer[at-v.footerStart:])
			written += n
		}
	}
	return written, nil
}
