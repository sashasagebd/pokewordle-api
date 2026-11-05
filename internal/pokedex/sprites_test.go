package pokedex

import (
	"bytes"
	"image/png"
	"testing"
)

func TestEverySpriteMakesASilhouette(t *testing.T) {
	if err := CheckSprites(); err != nil {
		t.Fatal(err)
	}
	for id := 1; id <= Count; id++ {
		b, err := Silhouette(id)
		if err != nil {
			t.Fatalf("#%d: %v", id, err)
		}
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("#%d: %v", id, err)
		}
		bounds := img.Bounds()
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				if r, g, b, a := img.At(x, y).RGBA(); r|g|b != 0 || (a != 0 && a != 0xffff) {
					t.Fatalf("#%d: pixel (%d, %d) isn't black or transparent", id, x, y)
				}
			}
		}
	}
	if _, err := Sprite(0); err == nil {
		t.Error("Sprite(0) found something")
	}
}
