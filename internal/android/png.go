package android

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"hash/adler32"
	"hash/crc32"
	"image"
	"image/jpeg"
	"io"
	"sync"
)

// encodePNG writes img as an 8-bit RGB PNG. It is a parallel encoder: rows are filtered
// (PNG "Up" filter) in bands, each band is deflated independently at BestSpeed and ended
// with a sync flush, and the bands are concatenated into one valid zlib stream (the pigz
// approach). The alpha channel of emulator frames is always opaque, so it is dropped.
func encodePNG(w io.Writer, img rgbaImage) error {
	W, H := img.W, img.H
	stride := 1 + W*3
	raw := make([]byte, H*stride)
	parallelRows(H, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			row := raw[y*stride : (y+1)*stride]
			src := img.Pix[y*W*4 : (y+1)*W*4]
			if y == 0 {
				row[0] = 0 // None
				for x, i := 0, 1; x < W; x++ {
					row[i], row[i+1], row[i+2] = src[x*4], src[x*4+1], src[x*4+2]
					i += 3
				}
				continue
			}
			prev := img.Pix[(y-1)*W*4 : y*W*4]
			row[0] = 2 // Up
			for x, i := 0, 1; x < W; x++ {
				o := x * 4
				row[i] = src[o] - prev[o]
				row[i+1] = src[o+1] - prev[o+1]
				row[i+2] = src[o+2] - prev[o+2]
				i += 3
			}
		}
	})

	// Deflate bands in parallel.
	nb := 8
	if H < 256 {
		nb = 1
	}
	band := (H + nb - 1) / nb
	type part struct {
		buf bytes.Buffer
		err error
	}
	parts := make([]*part, 0, nb)
	var wg sync.WaitGroup
	for y := 0; y < H; y += band {
		p := &part{}
		parts = append(parts, p)
		y0, y1 := y, min(H, y+band)
		last := y1 == H
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.buf.Grow((y1 - y0) * stride / 4)
			fw, _ := flate.NewWriter(&p.buf, flate.BestSpeed)
			if _, err := fw.Write(raw[y0*stride : y1*stride]); err != nil {
				p.err = err
				return
			}
			if last {
				p.err = fw.Close()
			} else {
				p.err = fw.Flush() // byte-aligned, non-final: the next band can follow
			}
		}()
	}
	var sum uint32
	done := make(chan struct{})
	go func() { sum = adler32.Checksum(raw); close(done) }()
	wg.Wait()
	<-done

	var idat bytes.Buffer
	size := 6
	for _, p := range parts {
		if p.err != nil {
			return p.err
		}
		size += p.buf.Len()
	}
	idat.Grow(size)
	idat.Write([]byte{0x78, 0x01}) // zlib header: deflate, 32K window, fastest
	for _, p := range parts {
		idat.Write(p.buf.Bytes())
	}
	var a [4]byte
	binary.BigEndian.PutUint32(a[:], sum)
	idat.Write(a[:])

	bw := &errWriter{w: w}
	bw.write([]byte("\x89PNG\r\n\x1a\n"))
	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:], uint32(W))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(H))
	ihdr[8], ihdr[9] = 8, 2 // 8-bit, truecolor RGB
	writeChunk(bw, "IHDR", ihdr[:])
	writeChunk(bw, "IDAT", idat.Bytes())
	writeChunk(bw, "IEND", nil)
	return bw.err
}

type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) write(b []byte) {
	if e.err == nil {
		_, e.err = e.w.Write(b)
	}
}

func writeChunk(w *errWriter, typ string, data []byte) {
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(len(data)))
	copy(hdr[4:], typ)
	w.write(hdr[:])
	w.write(data)
	c := crc32.NewIEEE()
	c.Write(hdr[4:])
	c.Write(data)
	var s [4]byte
	binary.BigEndian.PutUint32(s[:], c.Sum32())
	w.write(s[:])
}

func encodeJPEG(w io.Writer, img rgbaImage) error {
	m := &image.RGBA{Pix: img.Pix, Stride: img.W * 4, Rect: image.Rect(0, 0, img.W, img.H)}
	return jpeg.Encode(w, m, &jpeg.Options{Quality: 85})
}
