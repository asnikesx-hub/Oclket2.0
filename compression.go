package main

import (
	"bytes"
	"io"
	"sync"

	"github.com/pierrec/lz4/v4"
)

var lz4WriterPool = sync.Pool{
	New: func() any {
		return lz4.NewWriter(nil)
	},
}

var lz4ReaderPool = sync.Pool{
	New: func() any { return lz4.NewReader(nil) },
}

var compressBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 256*1024)
		return &b
	},
}

const compressionThreshold = 1024

// compressValue сжимает value через LZ4.
// Если значение меньше порога — возвращает как есть.
// Возвращает НОВЫЙ slice (аллокация неизбежна).
func compressValue(value []byte) ([]byte, error) {
	if len(value) < compressionThreshold {
		return value, nil
	}

	bufPtr := compressBufPool.Get().(*[]byte)
	buf := (*bufPtr)[:0]
	defer func() {
		*bufPtr = buf[:0]
		compressBufPool.Put(bufPtr)
	}()

	writer := lz4WriterPool.Get().(*lz4.Writer)
	defer lz4WriterPool.Put(writer)

	out := bytes.NewBuffer(buf)
	writer.Reset(out)

	if _, err := writer.Write(value); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}

	// out.Bytes() — view на buf. Копируем в новый slice,
	// потому что buf вернётся в пул и будет перезаписан.
	result := make([]byte, out.Len())
	copy(result, out.Bytes())
	return result, nil
}

// decompressValue распаковывает LZ4-данные.
// Если данные не LZ4 — возвращает как есть.
// Возвращает новый slice — вызывающий владеет им.
func decompressValue(value []byte) ([]byte, error) {
	// Быстрая проверка magic number LZ4: 0x04 0x22 0x4D 0x18
	if len(value) < 4 ||
		value[0] != 0x04 || value[1] != 0x22 ||
		value[2] != 0x4D || value[3] != 0x18 {
		return value, nil
	}

	reader := lz4ReaderPool.Get().(*lz4.Reader)
	defer lz4ReaderPool.Put(reader)

	in := bytes.NewReader(value)
	reader.Reset(in)

	estimated := len(value) * 4
	if estimated < 4096 {
		estimated = 4096
	}

	buf := make([]byte, 0, estimated)

	for {
		if len(buf) == cap(buf) {
			newCap := cap(buf) * 2
			if newCap == 0 {
				newCap = 4096
			}
			newBuf := make([]byte, len(buf), newCap)
			copy(newBuf, buf)
			buf = newBuf
		}
		n, err := reader.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}
