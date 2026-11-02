// Package thumb renders a small JPEG preview of an image using only the
// standard library's decoders. It exists for the upload store: a chat showing
// a 20 MB screenshot should not have to download and decode the original to
// draw a 14rem-wide chip. The source file is content-addressed and immutable,
// so a thumbnail keyed by the same hash is cached forever.
package thumb

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // register the decoders image.Decode uses
	"image/jpeg"
	_ "image/png" // (jpeg is registered by image/jpeg below)
	"os"
)

// MaxDim is the longest edge of a generated thumbnail. Big enough for a
// retina attachment chip, small enough that a page of them is cheap.
const MaxDim = 640

// maxPixels refuses a decompression bomb before image.Decode allocates its
// full-size pixel buffer. 60 megapixels is far beyond any screenshot or
// photo a chat attaches.
const maxPixels = 60_000_000

// Generate reads path and returns a JPEG no larger than maxDim on either edge.
// The source may be PNG, JPEG or GIF (the first frame of an animation). A file
// the standard library cannot decode (WebP, AVIF, a video) is an error, and the
// caller serves the original instead.
func Generate(path string, maxDim int) ([]byte, error) {
	if maxDim <= 0 {
		maxDim = MaxDim
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil, fmt.Errorf("decoding image header: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return nil, fmt.Errorf("image is %dx%d, too large to preview", cfg.Width, cfg.Height)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	src, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decoding image: %w", err)
	}

	out := scale(src, maxDim)
	// JPEG has no alpha; composite onto white so a transparent PNG's
	// transparent pixels don't turn black.
	flat := image.NewRGBA(out.Bounds())
	draw.Draw(flat, flat.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), out, out.Bounds().Min, draw.Over)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, flat, &jpeg.Options{Quality: 82}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// scale shrinks src to fit maxDim, averaging each destination pixel's source
// rectangle (a box filter). Good enough for a preview, and no dependency on
// golang.org/x/image. Images already within the bound are returned unchanged.
func scale(src image.Image, maxDim int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}
	if w <= maxDim && h <= maxDim {
		if rgba, ok := src.(*image.RGBA); ok {
			return rgba
		}
		out := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(out, out.Bounds(), src, b.Min, draw.Src)
		return out
	}

	dw, dh := w, h
	if w >= h {
		dw = maxDim
		dh = max(1, (h*maxDim+w/2)/w)
	} else {
		dh = maxDim
		dw = max(1, (w*maxDim+h/2)/h)
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		sy0 := b.Min.Y + y*h/dh
		sy1 := b.Min.Y + (y+1)*h/dh
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for x := 0; x < dw; x++ {
			sx0 := b.Min.X + x*w/dw
			sx1 := b.Min.X + (x+1)*w/dw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var r, g, bl, a, n uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					cr, cg, cb, ca := src.At(sx, sy).RGBA()
					r += uint64(cr)
					g += uint64(cg)
					bl += uint64(cb)
					a += uint64(ca)
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(r / n >> 8),
				G: uint8(g / n >> 8),
				B: uint8(bl / n >> 8),
				A: uint8(a / n >> 8),
			})
		}
	}
	return dst
}
