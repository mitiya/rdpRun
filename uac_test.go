package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"
	"time"
)

func setFrame(bmp *bitmapAccumulator, background, center byte) {
	bmp.mu.Lock()
	defer bmp.mu.Unlock()
	for y := 0; y < bmp.height; y++ {
		for x := 0; x < bmp.width; x++ {
			value := background
			if x >= bmp.width/3 && x < bmp.width*2/3 && y >= bmp.height/3 && y < bmp.height*2/3 {
				value = center
			}
			off := (y*bmp.width + x) * 4
			bmp.frame[off] = value
			bmp.frame[off+1] = value
			bmp.frame[off+2] = value
			bmp.frame[off+3] = 0xFF
		}
	}
	bmp.has = true
	bmp.revision++
}

func TestIsUACFrame(t *testing.T) {
	bmp := newBitmapAccumulator(90, 90)
	setFrame(bmp, 40, 40)
	baseline, ok := bmp.stats()
	if !ok {
		t.Fatal("stats reported no baseline frame")
	}

	setFrame(bmp, 80, 220)
	uac, ok := bmp.stats()
	if !ok || !isUACFrame(baseline, uac) {
		t.Fatal("isUACFrame did not recognize a protected desktop with centered dialog")
	}

	setFrame(bmp, 40, 220)
	localChange, _ := bmp.stats()
	if isUACFrame(baseline, localChange) {
		t.Fatal("isUACFrame accepted a local central change")
	}

	partialFrame := uac
	partialFrame.coverage = 0.5
	if isUACFrame(baseline, partialFrame) {
		t.Fatal("isUACFrame accepted an incomplete bitmap frame")
	}
}

func TestWatchUACRequiresTwoFreshFrames(t *testing.T) {
	bmp := newBitmapAccumulator(90, 90)
	setFrame(bmp, 40, 40)
	baseline, _ := bmp.stats()
	go func() {
		time.Sleep(25 * time.Millisecond)
		setFrame(bmp, 80, 220)
		time.Sleep(175 * time.Millisecond)
		setFrame(bmp, 80, 220)
	}()
	confirmed, _ := bmp.watchUAC(600*time.Millisecond, baseline, nil, nil, nil)
	if !confirmed {
		t.Fatal("watchUAC did not confirm two matching bitmap updates")
	}
}

func TestTemplateSimilarity(t *testing.T) {
	path := t.TempDir() + "/uac-reference.png"
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			shade := uint8((x*3 + y*5) % 256)
			img.SetRGBA(x, y, color.RGBA{R: shade, G: shade / 2, B: 255 - shade, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	template, err := loadUACTemplate(path)
	if err != nil {
		t.Fatal(err)
	}
	bmp := newBitmapAccumulator(1024, 768)
	originX := (bmp.width - img.Bounds().Dx()) / 2
	originY := (bmp.height - img.Bounds().Dy()) / 2
	bmp.mu.Lock()
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			off := ((originY+y)*bmp.width + originX + x) * 4
			pixel := img.RGBAAt(x, y)
			bmp.frame[off] = pixel.B
			bmp.frame[off+1] = pixel.G
			bmp.frame[off+2] = pixel.R
			bmp.frame[off+3] = 0xFF
		}
	}
	bmp.has = true
	bmp.mu.Unlock()

	similarity, ok := bmp.templateSimilarity(template)
	if !ok || similarity < 0.99 {
		t.Fatalf("template similarity = %.3f, want at least 0.99", similarity)
	}
}

func TestRunInputLanded(t *testing.T) {
	bmp := newBitmapAccumulator(1024, 768)
	// Uniform-bright frame = empty Run edit box.
	bmp.mu.Lock()
	for i := 0; i < len(bmp.frame); i += 4 {
		bmp.frame[i], bmp.frame[i+1], bmp.frame[i+2], bmp.frame[i+3] = 240, 240, 240, 0xFF
	}
	bmp.has = true
	bmp.mu.Unlock()

	before, ok := bmp.runEditBoxInk()
	if !ok {
		t.Fatal("runEditBoxInk reported no baseline")
	}
	if runInputLanded(before, before, 20) {
		t.Fatal("runInputLanded reported input on an unchanged (focus-stolen) box")
	}

	// Paint a few thin dark strokes in the edit-box band (as thin as glyphs,
	// which the old sparse-grid sampler stepped over): every 6th column.
	dlgW, dlgH, ox, oy := 431, 208, 15, 509
	x0 := ox + int(float64(dlgW)*runEditRelX0)
	x1 := ox + int(float64(dlgW)*runEditRelX1)
	y0 := oy + int(float64(dlgH)*runEditRelY0)
	y1 := oy + int(float64(dlgH)*runEditRelY1)
	bmp.mu.Lock()
	for y := y0; y < y1; y++ {
		for x := x0; x < x0+30 && x < x1; x += 6 {
			off := (y*bmp.width + x) * 4
			bmp.frame[off], bmp.frame[off+1], bmp.frame[off+2] = 30, 30, 30
		}
	}
	bmp.mu.Unlock()

	after, ok := bmp.runEditBoxInk()
	if !ok {
		t.Fatal("runEditBoxInk reported no post-typing count")
	}
	if !runInputLanded(before, after, 20) {
		t.Fatalf("runInputLanded missed typed text (ink %d -> %d)", before, after)
	}
}

func TestLoadDefaultUACTemplate(t *testing.T) {
	template, err := loadDefaultUACTemplate()
	if err != nil {
		t.Fatal(err)
	}
	if template.width != 456 || template.height != 310 {
		t.Fatalf("embedded template size = %dx%d, want 456x310", template.width, template.height)
	}
}

func TestRunDialogSimilarity(t *testing.T) {
	template, err := loadDefaultRunDialogTemplate()
	if err != nil {
		t.Fatal(err)
	}
	if template.width != 431 || template.height != 208 {
		t.Fatalf("embedded Run dialog size = %dx%d, want 431x208", template.width, template.height)
	}
	img, _, err := image.Decode(bytes.NewReader(defaultRunDialogTemplatePNG))
	if err != nil {
		t.Fatal(err)
	}
	bmp := newBitmapAccumulator(1024, 768)
	bmp.mu.Lock()
	for y := 0; y < template.height; y++ {
		for x := 0; x < template.width; x++ {
			pixel := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
			off := ((509+y)*bmp.width + 15 + x) * 4
			bmp.frame[off] = pixel.B
			bmp.frame[off+1] = pixel.G
			bmp.frame[off+2] = pixel.R
			bmp.frame[off+3] = 0xFF
		}
	}
	bmp.has = true
	bmp.mu.Unlock()

	similarity, ok := bmp.runDialogSimilarity(template)
	if !ok || similarity < 0.99 {
		t.Fatalf("Run dialog similarity = %.3f, want at least 0.99", similarity)
	}
}

func TestWatchRunDialogWaitsForInitialFrame(t *testing.T) {
	template, err := loadDefaultRunDialogTemplate()
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(defaultRunDialogTemplatePNG))
	if err != nil {
		t.Fatal(err)
	}
	bmp := newBitmapAccumulator(1024, 768)
	setRunDialogFrame := func() {
		bmp.mu.Lock()
		defer bmp.mu.Unlock()
		for y := 0; y < template.height; y++ {
			for x := 0; x < template.width; x++ {
				pixel := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
				off := ((509+y)*bmp.width + 15 + x) * 4
				bmp.frame[off] = pixel.B
				bmp.frame[off+1] = pixel.G
				bmp.frame[off+2] = pixel.R
				bmp.frame[off+3] = 0xFF
			}
		}
		bmp.has = true
		bmp.revision++
	}
	go func() {
		time.Sleep(25 * time.Millisecond)
		setRunDialogFrame()
		time.Sleep(175 * time.Millisecond)
		setRunDialogFrame()
	}()

	confirmed, _ := bmp.watchRunDialog(600*time.Millisecond, template, 0.72, nil)
	if !confirmed {
		t.Fatal("watchRunDialog did not confirm Run after delayed initial frames")
	}
}

// TestWatchRunDialogStaticFrame guards the regression where a Run dialog that
// is present but no longer redrawing (static composite, no new revisions) was
// never recognized because the watcher only evaluated on revision changes.
func TestWatchRunDialogStaticFrame(t *testing.T) {
	template, err := loadDefaultRunDialogTemplate()
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(defaultRunDialogTemplatePNG))
	if err != nil {
		t.Fatal(err)
	}
	bmp := newBitmapAccumulator(1024, 768)
	bmp.mu.Lock()
	for y := 0; y < template.height; y++ {
		for x := 0; x < template.width; x++ {
			pixel := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
			off := ((509+y)*bmp.width + 15 + x) * 4
			bmp.frame[off] = pixel.B
			bmp.frame[off+1] = pixel.G
			bmp.frame[off+2] = pixel.R
			bmp.frame[off+3] = 0xFF
		}
	}
	bmp.has = true
	bmp.revision++ // one update, then the frame stays static (no more revisions)
	bmp.mu.Unlock()

	confirmed, _ := bmp.watchRunDialog(2*time.Second, template, 0.72, nil)
	if !confirmed {
		t.Fatal("watchRunDialog did not confirm a present but static Run dialog")
	}
}

func TestReorderFlagsKeepsTemplatePath(t *testing.T) {
	args := reorderFlags([]string{"192.0.2.10:3389", "admin", "password", "whoami", "--uac-template", ".\\uac-reference.png", "--capture"})
	want := []string{"--uac-template", ".\\uac-reference.png", "--capture", "192.0.2.10:3389", "admin", "password", "whoami"}
	if len(args) != len(want) {
		t.Fatalf("reordered argument count = %d, want %d", len(args), len(want))
	}
	for index, value := range want {
		if args[index] != value {
			t.Fatalf("argument %d = %q, want %q", index, args[index], value)
		}
	}
}

func TestRunDialogThreshold(t *testing.T) {
	args := []string{"192.0.2.10:3389", "admin", "password", "whoami", "--run-dialog-threshold", "0.70"}
	cfg, err := parseArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunDialogThreshold != 0.70 {
		t.Fatalf("Run dialog threshold = %.2f, want 0.70", cfg.RunDialogThreshold)
	}

	maxCfg, err := parseArgs([]string{"192.0.2.10:3389", "admin", "password", "whoami", "--max-attempts", "3"})
	if err != nil {
		t.Fatal(err)
	}
	if maxCfg.MaxAttempts != 3 {
		t.Fatalf("max attempts = %d, want 3", maxCfg.MaxAttempts)
	}
	if _, err := parseArgs([]string{"192.0.2.10:3389", "admin", "password", "whoami", "--max-attempts", "0"}); err == nil {
		t.Fatal("--max-attempts 0 was accepted")
	}

	defaultCfg, err := parseArgs([]string{"192.0.2.10:3389", "admin", "password", "whoami"})
	if err != nil {
		t.Fatal(err)
	}
	if defaultCfg.MaxAttempts != 6 {
		t.Fatalf("default max attempts = %d, want 6", defaultCfg.MaxAttempts)
	}
	if defaultCfg.RunInputThreshold != 20 {
		t.Fatalf("default run-input-threshold = %d, want 20", defaultCfg.RunInputThreshold)
	}
	if defaultCfg.RunDialogThreshold != 0.72 {
		t.Fatalf("default Run dialog threshold = %.2f, want 0.72", defaultCfg.RunDialogThreshold)
	}
	if defaultCfg.DesktopTimeout != 120*time.Second {
		t.Fatalf("default desktop timeout = %s, want 2m0s", defaultCfg.DesktopTimeout)
	}

	for _, threshold := range []string{"0", "-0.1", "1.01"} {
		_, err := parseArgs([]string{"192.0.2.10:3389", "admin", "password", "whoami", "--run-dialog-threshold", threshold})
		if err == nil {
			t.Fatalf("threshold %q was accepted", threshold)
		}
	}
	for _, timeout := range []string{"0s", "-1s"} {
		_, err := parseArgs([]string{"192.0.2.10:3389", "admin", "password", "whoami", "--desktop-timeout", timeout})
		if err == nil {
			t.Fatalf("desktop timeout %q was accepted", timeout)
		}
	}
}
