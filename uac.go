package main

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"math"
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
	mu       sync.Mutex
	width    int
	height   int
	bpp      int
	frame    []byte // top-down BGRX, width*height*4 (approximate composite)
	has      bool
	revision uint64
}

type frameStats struct {
	brightness float64
	blue       float64
	variance   float64
	coverage   float64
	regions    [9]float64
	revision   uint64
}

const (
	uacTemplateWidth  = 32
	uacTemplateHeight = 24
	uacTemplateScore  = 0.72
	runTemplateScore  = 0.78
)

type uacTemplate struct {
	pixels [uacTemplateWidth * uacTemplateHeight]float64
	width  int
	height int
}

//go:embed uac-reference.png
var defaultUACTemplatePNG []byte

//go:embed run-dialog-reference.png
var defaultRunDialogTemplatePNG []byte

func loadUACTemplate(path string) (*uacTemplate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodeUACTemplate(data)
}

func loadDefaultUACTemplate() (*uacTemplate, error) {
	return decodeUACTemplate(defaultUACTemplatePNG)
}

func loadDefaultRunDialogTemplate() (*uacTemplate, error) {
	return decodeUACTemplate(defaultRunDialogTemplatePNG)
}

func decodeUACTemplate(data []byte) (*uacTemplate, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	template := &uacTemplate{width: img.Bounds().Dx(), height: img.Bounds().Dy()}
	for y := 0; y < uacTemplateHeight; y++ {
		for x := 0; x < uacTemplateWidth; x++ {
			sourceX := img.Bounds().Min.X + x*img.Bounds().Dx()/uacTemplateWidth
			sourceY := img.Bounds().Min.Y + y*img.Bounds().Dy()/uacTemplateHeight
			r, g, b, _ := img.At(sourceX, sourceY).RGBA()
			template.pixels[y*uacTemplateWidth+x] = (float64(r>>8) + float64(g>>8) + float64(b>>8)) / 3
		}
	}
	normalizeTemplate(&template.pixels)
	return template, nil
}

func normalizeTemplate(pixels *[uacTemplateWidth * uacTemplateHeight]float64) {
	var sum float64
	for _, value := range pixels {
		sum += value
	}
	mean := sum / float64(len(pixels))
	var squaredSum float64
	for index, value := range pixels {
		value -= mean
		pixels[index] = value
		squaredSum += value * value
	}
	deviation := math.Sqrt(squaredSum / float64(len(pixels)))
	if deviation == 0 {
		return
	}
	for index := range pixels {
		pixels[index] /= deviation
	}
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
	updated := false
	for _, r := range rects {
		data := r.BitmapDataStream
		if r.IsCompress() {
			data = core.Decompress(r.BitmapDataStream, int(r.Width), int(r.Height), bppBytes(r.BitsPerPixel))
		}
		b.composite(r, data)
		updated = updated || len(data) > 0
	}
	if updated {
		b.revision++
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

// meanBrightness returns the mean luminance (0..255) over received pixels in
// the current frame. Returns 0 if no frame has been received. Unreceived
// pixels must not affect this metric: bitmap updates can be partial, and their
// zero-initialized backing pixels would otherwise make the UAC threshold
// unstable.
func (b *bitmapAccumulator) meanBrightness() (float64, bool) {
	stats, ok := b.stats()
	return stats.brightness, ok
}

func (b *bitmapAccumulator) stats() (frameStats, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.has || len(b.frame) == 0 {
		return frameStats{}, false
	}

	var luminanceSum, luminanceSquaredSum, blueSum float64
	var count float64
	var regionSums [9]float64
	var regionCounts [9]float64
	for y := 0; y < b.height; y++ {
		for x := 0; x < b.width; x++ {
			off := (y*b.width + x) * 4
			if b.frame[off+3] == 0 {
				continue
			}
			luminance := (float64(b.frame[off]) + float64(b.frame[off+1]) + float64(b.frame[off+2])) / 3
			region := (y*3/b.height)*3 + x*3/b.width
			luminanceSum += luminance
			luminanceSquaredSum += luminance * luminance
			blueSum += float64(b.frame[off])
			regionSums[region] += luminance
			regionCounts[region]++
			count++
		}
	}
	if count == 0 {
		return frameStats{}, false
	}

	stats := frameStats{
		brightness: luminanceSum / count,
		blue:       blueSum / count,
		coverage:   count / float64(b.width*b.height),
		revision:   b.revision,
	}
	stats.variance = luminanceSquaredSum/count - stats.brightness*stats.brightness
	for region := range stats.regions {
		if regionCounts[region] > 0 {
			stats.regions[region] = regionSums[region] / regionCounts[region]
		}
	}
	return stats, true
}

func isUACFrame(baseline, current frameStats) bool {
	if current.coverage < 0.65 {
		return false
	}
	brightnessChange := math.Abs(current.brightness - baseline.brightness)
	blueChange := math.Abs(current.blue - baseline.blue)
	globalTransition := brightnessChange >= math.Max(20, baseline.brightness*0.35) || blueChange >= 30
	if !globalTransition {
		return false
	}
	changedRegions := 0
	for region := range current.regions {
		if math.Abs(current.regions[region]-baseline.regions[region]) >= 15 {
			changedRegions++
		}
	}
	if changedRegions < 6 {
		return false
	}

	center := current.regions[4]
	var edgeSum float64
	for _, region := range [...]int{0, 1, 2, 3, 5, 6, 7, 8} {
		edgeSum += current.regions[region]
	}
	edgeMean := edgeSum / 8
	return center >= edgeMean+18 && center >= current.brightness+12 && current.variance >= 250
}

func (b *bitmapAccumulator) templateSimilarity(template *uacTemplate) (float64, bool) {
	if template == nil {
		return 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.has {
		return 0, false
	}
	width := template.width * b.width / 1024
	height := template.height * b.height / 768
	centerX := (b.width - width) / 2
	centerY := (b.height - height) / 2
	return b.templateSimilarityNear(template, centerX, centerY, width, height, 6*max(1, b.width/128))
}

func (b *bitmapAccumulator) runDialogSimilarity(template *uacTemplate) (float64, bool) {
	if template == nil {
		return 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.has {
		return 0, false
	}
	width := template.width * b.width / 1024
	height := template.height * b.height / 768
	originX := 15 * b.width / 1024
	originY := 509 * b.height / 768
	return b.templateSimilarityNear(template, originX, originY, width, height, 4*max(1, b.width/128))
}

func (b *bitmapAccumulator) templateSimilarityNear(template *uacTemplate, originX, originY, width, height, searchRange int) (float64, bool) {
	if width <= 0 || height <= 0 || width > b.width || height > b.height {
		return 0, false
	}
	searchStep := max(1, b.width/128)
	bestSimilarity := 0.0
	found := false
	for offsetY := -searchRange; offsetY <= searchRange; offsetY += searchStep {
		for offsetX := -searchRange; offsetX <= searchRange; offsetX += searchStep {
			similarity, ok := b.templateSimilarityAt(template, originX+offsetX, originY+offsetY, width, height)
			if ok && (!found || similarity > bestSimilarity) {
				bestSimilarity = similarity
				found = true
			}
		}
	}
	return bestSimilarity, found
}

func (b *bitmapAccumulator) templateSimilarityAt(template *uacTemplate, originX, originY, width, height int) (float64, bool) {
	if originX < 0 || originY < 0 || originX+width > b.width || originY+height > b.height {
		return 0, false
	}
	var candidate [uacTemplateWidth * uacTemplateHeight]float64
	for y := 0; y < uacTemplateHeight; y++ {
		for x := 0; x < uacTemplateWidth; x++ {
			sourceX := originX + x*width/uacTemplateWidth
			sourceY := originY + y*height/uacTemplateHeight
			off := (sourceY*b.width + sourceX) * 4
			if b.frame[off+3] == 0 {
				return 0, false
			}
			candidate[y*uacTemplateWidth+x] = (float64(b.frame[off]) + float64(b.frame[off+1]) + float64(b.frame[off+2])) / 3
		}
	}
	normalizeTemplate(&candidate)
	var correlation float64
	for index, value := range candidate {
		correlation += value * template.pixels[index]
	}
	return (correlation/float64(len(candidate)) + 1) / 2, true
}

// watchUAC runs a frame-based watcher for up to timeout. It recognizes a UAC
// prompt only after a global screen transition and a centered modal-dialog
// signature persist across two distinct bitmap updates.
func (b *bitmapAccumulator) watchUAC(timeout time.Duration, baseline frameStats, template *uacTemplate, onSample func(float64), onConfirm func(float64)) (bool, float64) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	matchingFrames := 0
	lastRevision := baseline.revision
	for {
		select {
		case <-ticker.C:
			current, ok := b.stats()
			if !ok || current.revision == lastRevision {
				continue
			}
			lastRevision = current.revision
			similarity := 0.0
			matches := isUACFrame(baseline, current)
			if template != nil {
				var ok bool
				similarity, ok = b.templateSimilarity(template)
				matches = ok && similarity >= uacTemplateScore
				if ok && onSample != nil {
					onSample(similarity)
				}
			}
			if matches {
				matchingFrames++
			} else {
				matchingFrames = 0
			}
			if matchingFrames >= 2 {
				if onConfirm != nil {
					onConfirm(similarity)
				}
				return true, similarity
			}
		case <-time.After(time.Until(deadline)):
			return false, 0
		}
	}
}

func (b *bitmapAccumulator) watchRunDialog(timeout time.Duration, template *uacTemplate, onSample func(float64)) (bool, float64) {
	baseline, ok := b.stats()
	if !ok {
		return false, 0
	}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	matchingFrames := 0
	lastRevision := baseline.revision
	for {
		select {
		case <-ticker.C:
			current, ok := b.stats()
			if !ok || current.revision == lastRevision {
				continue
			}
			lastRevision = current.revision
			similarity, matches := b.runDialogSimilarity(template)
			if onSample != nil {
				onSample(similarity)
			}
			if matches && similarity >= runTemplateScore {
				matchingFrames++
			} else {
				matchingFrames = 0
			}
			if matchingFrames >= 2 {
				return true, similarity
			}
		case <-time.After(time.Until(deadline)):
			return false, 0
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
