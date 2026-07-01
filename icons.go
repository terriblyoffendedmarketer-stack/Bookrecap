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
		cachedIcon192 = makePurpleSquare(192)
		cachedIcon512 = makePurpleSquare(512)
	})
}

func makePurpleSquare(size int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	bg := color.NRGBA{R: 124, G: 106, B: 247, A: 255}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.SetNRGBA(x, y, bg)
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
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
