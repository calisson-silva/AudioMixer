package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

type rgba struct{ r, g, b, a float64 }

func main() {
	dir := "assets/icons"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal(err)
	}

	var icoImages [][]byte
	for _, size := range []int{16, 24, 32, 48, 64, 128, 256} {
		data, err := encodePNG(render(size, 4, appStyle))
		if err != nil {
			fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("audiomixer-%d.png", size)), data, 0o644); err != nil {
			fatal(err)
		}
		icoImages = append(icoImages, data)
		if size == 256 {
			if err = os.WriteFile(filepath.Join(dir, "audiomixer.png"), data, 0o644); err != nil {
				fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "audiomixer.ico"), makeICO(icoImages, []int{16, 24, 32, 48, 64, 128, 256}), 0o644); err != nil {
		fatal(err)
	}
	for name, style := range map[string]style{
		"tray-light.png": {tray: true, trayColor: rgba{r: 0.08, g: 0.17, b: 0.32, a: 1}},
		"tray-dark.png":  {tray: true, trayColor: rgba{r: 0.88, g: 0.95, b: 1, a: 1}},
	} {
		data, err := encodePNG(render(32, 4, style))
		if err != nil {
			fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			fatal(err)
		}
	}
}

type style struct {
	tray      bool
	trayColor rgba
}

var appStyle = style{}

func render(size, scale int, s style) *image.RGBA {
	high := size * scale
	src := image.NewNRGBA(image.Rect(0, 0, high, high))
	for y := 0; y < high; y++ {
		for x := 0; x < high; x++ {
			nx := (float64(x) + 0.5) / float64(high)
			ny := (float64(y) + 0.5) / float64(high)
			var c rgba
			if s.tray {
				c = trayPixel(nx, ny, s.trayColor)
			} else {
				c = appPixel(nx, ny)
			}
			src.SetNRGBA(x, y, color.NRGBA{
				R: uint8(clamp(c.r, 0, 1) * 255),
				G: uint8(clamp(c.g, 0, 1) * 255),
				B: uint8(clamp(c.b, 0, 1) * 255),
				A: uint8(clamp(c.a, 0, 1) * 255),
			})
		}
	}

	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var sum rgba
			for sy := 0; sy < scale; sy++ {
				for sx := 0; sx < scale; sx++ {
					p := src.NRGBAAt(x*scale+sx, y*scale+sy)
					a := float64(p.A) / 255
					sum.r += float64(p.R) / 255 * a
					sum.g += float64(p.G) / 255 * a
					sum.b += float64(p.B) / 255 * a
					sum.a += a
				}
			}
			count := float64(scale * scale)
			alpha := sum.a / count
			if alpha > 0 {
				dst.SetRGBA(x, y, color.RGBA{
					R: uint8(sum.r / count / alpha * 255),
					G: uint8(sum.g / count / alpha * 255),
					B: uint8(sum.b / count / alpha * 255),
					A: uint8(alpha * 255),
				})
			}
		}
	}
	return dst
}

func appPixel(x, y float64) rgba {
	var c rgba
	if roundedRect(x, y, 0.04, 0.04, 0.96, 0.96, 0.25) {
		t := clamp((x+y)*0.5, 0, 1)
		c = mix(rgba{r: 0.035, g: 0.065, b: 0.12, a: 1}, rgba{r: 0.075, g: 0.16, b: 0.29, a: 1}, t)
	}
	centers := [...]float64{0.26, 0.38, 0.50, 0.62, 0.74}
	heights := [...]float64{0.28, 0.46, 0.62, 0.46, 0.28}
	for i, cx := range centers {
		h := heights[i]
		if roundedRect(x, y, cx-0.052, 0.5-h/2, cx+0.052, 0.5+h/2, 0.052) {
			c = mix(rgba{r: 0.25, g: 0.84, b: 1, a: 1}, rgba{r: 0.38, g: 0.46, b: 1, a: 1}, float64(i)/4)
		}
	}
	return c
}

func trayPixel(x, y float64, c rgba) rgba {
	centers := [...]float64{0.19, 0.345, 0.50, 0.655, 0.81}
	heights := [...]float64{0.36, 0.60, 0.84, 0.60, 0.36}
	for i, cx := range centers {
		h := heights[i]
		if roundedRect(x, y, cx-0.055, 0.84-h, cx+0.055, 0.84, 0.055) {
			return c
		}
	}
	return rgba{}
}

func roundedRect(x, y, left, top, right, bottom, radius float64) bool {
	if x < left || x > right || y < top || y > bottom {
		return false
	}
	cx := clamp(x, left+radius, right-radius)
	cy := clamp(y, top+radius, bottom-radius)
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= radius*radius
}

func mix(a, b rgba, t float64) rgba {
	return rgba{
		r: a.r + (b.r-a.r)*t,
		g: a.g + (b.g-a.g)*t,
		b: a.b + (b.b-a.b)*t,
		a: a.a + (b.a-a.a)*t,
	}
}

func clamp(v, low, high float64) float64 { return math.Max(low, math.Min(high, v)) }

func encodePNG(img image.Image) ([]byte, error) {
	var out bytes.Buffer
	err := png.Encode(&out, img)
	return out.Bytes(), err
}

func makeICO(images [][]byte, sizes []int) []byte {
	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, uint16(0))
	_ = binary.Write(&out, binary.LittleEndian, uint16(1))
	_ = binary.Write(&out, binary.LittleEndian, uint16(len(images)))
	offset := uint32(6 + len(images)*16)
	for i, data := range images {
		size := sizes[i]
		width, height := byte(size), byte(size)
		if size == 256 {
			width, height = 0, 0
		}
		out.Write([]byte{width, height, 0, 0})
		_ = binary.Write(&out, binary.LittleEndian, uint16(1))
		_ = binary.Write(&out, binary.LittleEndian, uint16(32))
		_ = binary.Write(&out, binary.LittleEndian, uint32(len(data)))
		_ = binary.Write(&out, binary.LittleEndian, offset)
		offset += uint32(len(data))
	}
	for _, data := range images {
		out.Write(data)
	}
	return out.Bytes()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
