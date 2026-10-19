package publiccard

import (
	"bytes"
	"image/png"
	"os"
	"strings"
	"testing"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

func TestRender(t *testing.T) {
	b, err := Render(Card{Name: "Rodolfo de Bonis da Silva Sauro Comprido", Platinums: 1042, Trophies: 123456, Rare: 310, Badges: 7}, "trophy.rodolfodebonis.com.br/u/rodolfo")
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != Width || img.Bounds().Dy() != Height {
		t.Fatalf("tamanho %v", img.Bounds())
	}
	if out := os.Getenv("CARD_OUT"); out != "" {
		_ = os.WriteFile(out, b, 0o600)
	}
}

func TestHelpers(t *testing.T) {
	if thousands(12345) != "12.345" || thousands(7) != "7" {
		t.Fatal("thousands")
	}
	facesOnce.Do(loadFaces)
	long := fitWidth(faces["name"], "RODOLFO DE BONIS DA SILVA SAURO COMPRIDO", 980)
	if !strings.HasSuffix(long, "…") || font.MeasureString(faces["name"], long) > fixed.I(980) {
		t.Fatalf("fitWidth: %q", long)
	}
	if fitWidth(faces["name"], "ANA", 980) != "ANA" {
		t.Fatal("nome curto não corta")
	}
}
