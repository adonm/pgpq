package chunk

import (
	"bytes"

	"github.com/klauspost/compress/zstd"
)

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

var (
	zstdEncoder = newEncoder()
	zstdDecoder = newDecoder()
)

func newEncoder() *zstd.Encoder {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))
	if err != nil {
		panic(err)
	}
	return enc
}

func newDecoder() *zstd.Decoder {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		panic(err)
	}
	return dec
}

func Compress(data []byte) []byte {
	return zstdEncoder.EncodeAll(data, nil)
}

func Decompress(data []byte) ([]byte, error) {
	if !IsCompressed(data) {
		return data, nil
	}
	return zstdDecoder.DecodeAll(data, nil)
}

func IsCompressed(data []byte) bool {
	return bytes.HasPrefix(data, zstdMagic)
}
