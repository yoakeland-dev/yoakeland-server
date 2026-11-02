package thumb

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func writePNG(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGenerateShrinksToMaxDim(t *testing.T) {
	p := writePNG(t, 1600, 900)
	data, err := Generate(p, 640)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("thumbnail is not a JPEG: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != 640 || b.Dy() != 360 {
		t.Fatalf("thumbnail is %dx%d, want 640x360", b.Dx(), b.Dy())
	}
}

func TestGenerateKeepsSmallImageSize(t *testing.T) {
	p := writePNG(t, 120, 80)
	data, err := Generate(p, 640)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 120 || b.Dy() != 80 {
		t.Fatalf("small image resized to %dx%d", b.Dx(), b.Dy())
	}
}

func TestGenerateRejectsNonImage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(p, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(p, 640); err == nil {
		t.Fatal("non-image decoded")
	}
}
