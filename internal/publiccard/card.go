// Package publiccard desenha o cartão de prévia do perfil público (1200×630, formato do Open Graph):
// é a imagem que aparece ao colar o link no WhatsApp/Discord. Puro (sem rede): fontes Go embutidas.
package publiccard

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Width/Height do cartão (proporção recomendada do og:image).
const (
	Width  = 1200
	Height = 630
)

// Card é o que vai no cartão.
type Card struct {
	Name      string
	Platinums int // platina PSN + 100% Steam
	Trophies  int
	Rare      int
	Badges    int
}

var (
	bg     = color.RGBA{7, 8, 13, 255}
	panel  = color.RGBA{18, 20, 31, 255}
	cyan   = color.RGBA{61, 245, 255, 255}
	violet = color.RGBA{139, 92, 246, 255}
	text1  = color.RGBA{240, 244, 252, 255}
	text3  = color.RGBA{138, 147, 166, 255}
	gold   = color.RGBA{245, 197, 66, 255}

	facesOnce sync.Once
	faces     map[string]font.Face
	facesErr  error
)

func loadFaces() {
	faces = map[string]font.Face{}
	for name, spec := range map[string]struct {
		ttf  []byte
		size float64
	}{
		"kicker": {gobold.TTF, 28}, "name": {gobold.TTF, 80}, "num": {gobold.TTF, 60},
		"label": {goregular.TTF, 26}, "foot": {goregular.TTF, 26},
	} {
		f, err := opentype.Parse(spec.ttf)
		if err != nil {
			facesErr = err
			return
		}
		face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: spec.size, DPI: 72, Hinting: font.HintingFull})
		if err != nil {
			facesErr = err
			return
		}
		faces[name] = face
	}
}

// Render desenha o PNG.
func Render(c Card, footer string) ([]byte, error) {
	facesOnce.Do(loadFaces)
	if facesErr != nil {
		return nil, facesErr
	}
	img := image.NewRGBA(image.Rect(0, 0, Width, Height))
	draw.Draw(img, img.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)
	// Faixa de brilho no topo (ciano → violeta) e painel central com canto chanfrado.
	for x := 0; x < Width; x++ {
		t := float64(x) / Width
		col := color.RGBA{lerp(cyan.R, violet.R, t), lerp(cyan.G, violet.G, t), lerp(cyan.B, violet.B, t), 255}
		for y := 0; y < 8; y++ {
			img.SetRGBA(x, y, col)
		}
	}
	fillChamfer(img, image.Rect(60, 70, Width-60, Height-70), 36, panel)

	write(img, "kicker", cyan, 110, 150, "TROPHY HUNTER")
	write(img, "name", text1, 110, 250, fitWidth(faces["name"], strings.ToUpper(c.Name), Width-110-110))

	stats := []struct {
		label string
		value int
		col   color.RGBA
	}{
		{"platinas", c.Platinums, cyan}, {"troféus", c.Trophies, text1}, {"raros (<5%)", c.Rare, violet}, {"medalhas", c.Badges, gold},
	}
	colW := (Width - 220) / len(stats)
	for i, s := range stats {
		x := 110 + i*colW
		write(img, "num", s.col, x, 410, fitWidth(faces["num"], thousands(s.value), colW-24))
		write(img, "label", text3, x, 455, s.label)
	}
	write(img, "foot", text3, 110, 525, footer)

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func write(img *image.RGBA, face string, col color.RGBA, x, y int, s string) {
	d := &font.Drawer{Dst: img, Src: &image.Uniform{col}, Face: faces[face], Dot: fixed.P(x, y)}
	d.DrawString(s)
}

func fillChamfer(img *image.RGBA, r image.Rectangle, c int, col color.RGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if (x-r.Min.X)+(y-r.Min.Y) < c || (r.Max.X-1-x)+(r.Max.Y-1-y) < c {
				continue // cantos superior-esquerdo e inferior-direito cortados (estilo do app)
			}
			img.SetRGBA(x, y, col)
		}
	}
}

func lerp(a, b uint8, t float64) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*t) }

// fitWidth corta o texto (com "…") até caber em maxPx na fonte dada.
func fitWidth(face font.Face, s string, maxPx int) string {
	limit := fixed.I(maxPx)
	if font.MeasureString(face, s) <= limit {
		return s
	}
	r := []rune(s)
	for len(r) > 1 {
		r = r[:len(r)-1]
		cand := strings.TrimRight(string(r), " ") + "…"
		if font.MeasureString(face, cand) <= limit {
			return cand
		}
	}
	return "…"
}

func thousands(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "." + s[i:]
	}
	return s
}
