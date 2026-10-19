// Package tickets draws a ticket's QR code and the printable PDF.
package tickets

import (
	"bytes"
	"image"
	"image/color"
	"image/png"

	"github.com/boombuler/barcode/qr"
)

// Matrix is a square QR code, one entry per module.
type Matrix struct {
	Size int
	dark []bool
}

// Dark reports whether the module at (x, y) is dark. Outside the code is light.
func (m Matrix) Dark(x, y int) bool {
	if x < 0 || y < 0 || x >= m.Size || y >= m.Size {
		return false
	}
	return m.dark[y*m.Size+x]
}

// QR encodes value in byte mode (8-bit, UTF-8) at error correction level M.
func QR(value string) (Matrix, error) {
	code, err := qr.Encode(value, qr.M, qr.Unicode) // byte mode: the library calls it Unicode
	if err != nil {
		return Matrix{}, err
	}
	n := code.Bounds().Dx()
	m := Matrix{Size: n, dark: make([]bool, n*n)}
	for y := range n {
		for x := range n {
			r, _, _, _ := code.At(x, y).RGBA()
			m.dark[y*n+x] = r == 0
		}
	}
	return m, nil
}

const (
	pxPerModule = 8
	quietZone   = 4
)

// PNG draws value's QR code black on white, 8 px per module, with a 4-module quiet zone.
func PNG(value string) ([]byte, error) {
	m, err := QR(value)
	if err != nil {
		return nil, err
	}
	side := (m.Size + 2*quietZone) * pxPerModule
	img := image.NewGray(image.Rect(0, 0, side, side))
	for y := range side {
		for x := range side {
			c := color.Gray{Y: 255}
			if m.Dark(x/pxPerModule-quietZone, y/pxPerModule-quietZone) {
				c.Y = 0
			}
			img.SetGray(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
