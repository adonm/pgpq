package layer

import (
	"context"
	"errors"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/fgb"
)

type cellFile struct {
	layout *fgb.Layout
	fence  *fgb.Fence
	chunk  *chunk.Chunk
	cols   []chunk.Column
}

func cellFileSize(c *cellFile) int64 {
	return int64(c.layout.Count())*64 + int64(c.fence.Len())*16 + 4096
}

func (l *Layer) cellFile(ctx context.Context, fileID, fenceFile int64) (*cellFile, error) {
	if c, ok := l.files.Get(fileID); ok {
		return c, nil
	}
	reader, err := l.blocks.Open(ctx, fileID)
	if err != nil {
		return nil, stale(err)
	}
	layout, err := fgb.ReadLayout(reader, reader.Size())
	if err != nil {
		return nil, err
	}
	if fenceFile == 0 {
		return nil, errors.New("layer: cell has no key fence; reload the collection")
	}
	fenceReader, err := l.blocks.Open(ctx, fenceFile)
	if err != nil {
		return nil, stale(err)
	}
	fence, err := fgb.ReadFence(fenceReader, fenceReader.Size())
	if err != nil {
		return nil, err
	}
	c := &cellFile{
		layout: layout,
		fence:  fence,
		chunk:  chunk.HeaderOnly(layout.Header()),
		cols:   chunk.HeaderColumns(layout.Header()),
	}
	l.files.Add(fileID, c)
	return c, nil
}
