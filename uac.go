package main

import (
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
	"sync"
	"time"

	"github.com/tomatome/grdp/core"
	"github.com/tomatome/grdp/protocol/pdu"
)

// bitmapAccumulator accumulates incoming bitmap tiles into a full-frame
// buffer and computes a mean-brightness metric used to detect the UAC secure
// desktop (which dims the whole screen to ~40% brightness).
type bitmapAccumulator struct {
	mu     sync.Mutex
	width  int
	height int
	bpp    int
	frame  []byte // top-down BGRX, width*height*4 (approximate composite)
	last   float64
	has    bool
}

func newBitmapAccumulator(width, height int) *bitmapAccumulator {
	return &bitmapAccumulator{
		width:  width,
		height: height,
		bpp:    4,
		frame:  make([]byte, width*height*4),
	}
}

// update composites a batch of bitmap tiles into the frame buffer.
func (b *bitmapAccumulator) update(rects []pdu.BitmapData) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range rects {
		data := r.BitmapDataStream
		if r.IsCompress() {
			data = core.Decompress(r.BitmapDataStream, int(r.Width), int(r.Height), bppBytes(r.BitsPerPixel))
		}
		b.composite(r, data)
	}
}

// composite blits a single tile into the frame buffer. We only need a rough
// brightness metric, so we handle 15/16/24/32bpp by sampling bytes.
func (b *bitmapAccumulator) composite(r pdu.BitmapData, data []byte) {
	if len(data) == 0 {
		return
	}
	bpp := bppBytes(r.BitsPerPixel)
	if bpp < 1 {
		bpp = 1
	}
	w := int(r.Width)
	h := int(r.Height)
	if w <= 0 || h <= 0 {
		return
	}
	// Bitmap data is bottom-up; copy rows into the frame buffer at the
	// destination rectangle. We expand to 4 bytes/pixel (BGRX) for a uniform
	// sampling buffer.
	dstX := int(r.DestLeft)
	dstY := int(r.DestTop)
	stride := w * bpp
	for y := 0; y < h; y++ {
		srcY := y // data is top-down in this library's representation
		srcOff := srcY * stride
		dstRow := dstY + y
		if dstRow < 0 || dstRow >= b.height {
			continue
		}
		for x := 0; x < w; x++ {
			dstCol := dstX + x
			if dstCol < 0 || dstCol >= b.width {
				continue
			}
			srcOffX := srcOff + x*bpp
			if srcOffX+bpp > len(data) {
				continue
			}
			var bb, gg, rr byte
			switch bpp {
			case 1:
				c := data[srcOffX]
				bb = c
				gg = c
				rr = c
			case 2:
				// 16bpp RGB555/565: low byte, high byte (little endian)
				lo := data[srcOffX]
				hi := data[srcOffX+1]
				_ = hi
				bb = lo
				gg = lo
				rr = lo
			default: // 3 or 4 bytes: BGR(A) order
				bb = data[srcOffX]
				gg = data[srcOffX+1]
				rr = data[srcOffX+2]
			}
			off := (dstRow*b.width + dstCol) * 4
			if off+3 < len(b.frame) {
				b.frame[off] = bb
				b.frame[off+1] = gg
				b.frame[off+2] = rr
				b.frame[off+3] = 0xFF
				b.has = true
			}
		}
	}
}

// meanBrightness returns the mean luminance (0..255) over the current frame,
// sampling pixels for speed. Returns 0 if no frame has been received.
func (b *bitmapAccumulator) meanBrightness() (float64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.has || len(b.frame) == 0 {
		return 0, false
	}
	const step = 4096 // sample every ~4KB to keep it cheap
	var sum, count float64
	for i := 0; i+2 < len(b.frame); i += step {
		// BGR order; approximate luminance as average of B,G,R.
		lum := float64(b.frame[i]) + float64(b.frame[i+1]) + float64(b.frame[i+2])
		sum += lum / 3
		count++
	}
	if count == 0 {
		return 0, false
	}
	return sum / count, true
}

// watchUAC runs a brightness watcher for up to timeout. If the mean brightness
// drops to <= dimRatio of the baseline (secure desktop dimming), it calls
// onConfirm once and returns. Returns whether a dim was detected.
func (b *bitmapAccumulator) watchUAC(timeout time.Duration, baseline float64, dimRatio float64, onConfirm func()) bool {
	deadline := time.Now().Add(timeout)
	if baseline <= 0 {
		baseline = 128 // default assumption if no baseline measured
	}
	threshold := baseline * dimRatio
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			cur, ok := b.meanBrightness()
			if !ok {
				continue
			}
			if cur <= threshold && cur > 0 {
				if onConfirm != nil {
					onConfirm()
				}
				return true
			}
		case <-time.After(time.Until(deadline)):
			return false
		}
	}
}

// unused but kept to silence the color import if needed.
var _ = uint16(0)

// savePNG writes the current accumulated frame to a PNG file for diagnostics.
// The frame buffer is BGRX; we convert to RGBA for the standard image package.
func (b *bitmapAccumulator) savePNG(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.has || len(b.frame) == 0 {
		return nil
	}
	img := image.NewRGBA(image.Rect(0, 0, b.width, b.height))
	for y := 0; y < b.height; y++ {
		for x := 0; x < b.width; x++ {
			off := (y*b.width + x) * 4
			if off+3 >= len(b.frame) {
				break
			}
			img.SetRGBA(x, y, color.RGBA{
				R: b.frame[off+2], // BGR -> RGB
				G: b.frame[off+1],
				B: b.frame[off+0],
				A: 0xFF,
			})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// keep the binary package referenced to avoid unused import in some builds.
var _ = binary.LittleEndian

// bppBytes converts bits-per-pixel to bytes-per-pixel using the upstream
// Bpp() rules (15->1, 16->2, 24->3, 32->4).
func bppBytes(bitsPerPixel uint16) int {
	switch bitsPerPixel {
	case 15:
		return 1
	case 16:
		return 2
	case 24:
		return 3
	case 32:
		return 4
	}
	return int(bitsPerPixel / 8)
}
