package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"sync"
)

var (
	cachedIcon192 []byte
	cachedIcon512 []byte
	iconOnce      sync.Once
)

func initIcons() {
	iconOnce.Do(func() {
		cachedIcon192 = makeBookIcon(192)
		cachedIcon512 = makeBookIcon(512)
	})
}

// makeBookIcon renders a simplified version of frontend/icon.svg — two
// overlapping open books on a rounded purple background — directly as a PNG
// via image/draw-style primitives, since the app has no SVG rasterizer or
// font-rendering dependency. Coordinates are scaled from the SVG's 512x512
// space, and layered in the same order the SVG paints them (white book
// first, then the lighter purple book on top).
func makeBookIcon(size int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	scale := float64(size) / 512

	bg := color.NRGBA{R: 124, G: 106, B: 247, A: 255}
	fillRoundedRect(img, 0, 0, float64(size), float64(size), scale*96, bg)

	white := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	darkA := color.NRGBA{R: 61, G: 52, B: 104, A: 255}
	drawBookIcon(img, scale, 136, 112, 168, 224, white, darkA, 1.0)

	purple := color.NRGBA{R: 157, G: 148, B: 248, A: 255}
	darkB := color.NRGBA{R: 42, G: 32, B: 96, A: 255}
	drawBookIcon(img, scale, 208, 112, 168, 224, purple, darkB, 0.9)

	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

// drawBookIcon draws one "open book" glyph: an outer cover, a darker inner
// page area, and a few horizontal lines standing in for text.
func drawBookIcon(img *image.NRGBA, scale, x, y, w, h float64, cover, page color.NRGBA, lineOpacity float64) {
	fillRoundedRect(img, x*scale, y*scale, w*scale, h*scale, 12*scale, cover)
	fillRoundedRect(img, (x+16)*scale, (y+16)*scale, (w-32)*scale, (h-32)*scale, 8*scale, page)

	lineColor := color.NRGBA{R: 255, G: 255, B: 255, A: uint8(255 * lineOpacity)}
	lineWidths := []float64{104, 86, 96, 78, 90}
	ly := y + 48
	for _, lw := range lineWidths {
		fillRoundedRect(img, (x+32)*scale, ly*scale, lw*scale, 10*scale, 5*scale, lineColor)
		ly += 24
	}
}

// fillRoundedRect draws an axis-aligned rounded rectangle by testing each
// pixel's distance from the nearest rounded-corner center — simple and fast
// enough for a one-time, cached icon render, alpha-blended onto whatever is
// already drawn so overlapping shapes composite correctly.
func fillRoundedRect(img *image.NRGBA, x, y, w, h, r float64, c color.NRGBA) {
	bounds := img.Bounds()
	x0, y0 := int(x), int(y)
	x1, y1 := int(x+w)+1, int(y+h)+1
	if x0 < bounds.Min.X {
		x0 = bounds.Min.X
	}
	if y0 < bounds.Min.Y {
		y0 = bounds.Min.Y
	}
	if x1 > bounds.Max.X {
		x1 = bounds.Max.X
	}
	if y1 > bounds.Max.Y {
		y1 = bounds.Max.Y
	}

	for py := y0; py < y1; py++ {
		for px := x0; px < x1; px++ {
			fx, fy := float64(px)+0.5, float64(py)+0.5
			if insideRoundedRect(fx, fy, x, y, w, h, r) {
				blendPixel(img, px, py, c)
			}
		}
	}
}

func insideRoundedRect(px, py, x, y, w, h, r float64) bool {
	if px < x || px > x+w || py < y || py > y+h {
		return false
	}
	cx, cy := px, py
	if cx < x+r {
		cx = x + r
	} else if cx > x+w-r {
		cx = x + w - r
	}
	if cy < y+r {
		cy = y + r
	} else if cy > y+h-r {
		cy = y + h - r
	}
	dx, dy := px-cx, py-cy
	return dx*dx+dy*dy <= r*r
}

func blendPixel(img *image.NRGBA, x, y int, c color.NRGBA) {
	if c.A == 255 {
		img.SetNRGBA(x, y, c)
		return
	}
	bg := img.NRGBAAt(x, y)
	a := float64(c.A) / 255
	blend := func(fg, bgv uint8) uint8 {
		return uint8(float64(fg)*a + float64(bgv)*(1-a))
	}
	img.SetNRGBA(x, y, color.NRGBA{
		R: blend(c.R, bg.R),
		G: blend(c.G, bg.G),
		B: blend(c.B, bg.B),
		A: 255,
	})
}

func handleIcon192(w http.ResponseWriter, r *http.Request) {
	initIcons()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(cachedIcon192)
}

func handleIcon512(w http.ResponseWriter, r *http.Request) {
	initIcons()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(cachedIcon512)
}
