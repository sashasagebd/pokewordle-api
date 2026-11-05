package pokedex

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
)

// The sprites are PokéAPI's default front sprites (96×96 PNGs), downloaded
// once by cmd/genseed and embedded, so the API never fetches them at runtime.
//
//go:embed sprites/*.png
var spriteFiles embed.FS

// Sprite returns Pokémon id's sprite as a PNG.
func Sprite(id int) ([]byte, error) {
	if id < 1 || id > Count {
		return nil, ErrNotFound
	}
	return spriteFiles.ReadFile(fmt.Sprintf("sprites/%d.png", id))
}

// CheckSprites checks that every Pokémon has a sprite, so a missing file
// fails at startup instead of in the middle of a game.
func CheckSprites() error {
	for id := 1; id <= Count; id++ {
		b, err := Sprite(id)
		if err != nil {
			return fmt.Errorf("sprite #%d: %w", id, err)
		}
		if _, err := png.DecodeConfig(bytes.NewReader(b)); err != nil {
			return fmt.Errorf("sprite #%d: %w", id, err)
		}
	}
	return nil
}

// Silhouette returns Pokémon id's silhouette as a PNG.
func Silhouette(id int) ([]byte, error) {
	b, err := Sprite(id)
	if err != nil {
		return nil, err
	}
	return MakeSilhouette(b)
}

// silhouettePalette is transparent and black: the output carries nothing of
// the sprite but its shape.
var silhouettePalette = color.Palette{color.Transparent, color.Black}

// MakeSilhouette turns a sprite into a black silhouette of the same size:
// every pixel that's at least half opaque becomes black, the rest
// transparent. The PNG is encoded from scratch, so none of the original
// file's colours or metadata survive.
func MakeSilhouette(sprite []byte) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(sprite))
	if err != nil {
		return nil, err
	}
	bounds := img.Bounds()
	out := image.NewPaletted(image.Rect(0, 0, bounds.Dx(), bounds.Dy()), silhouettePalette)
	filled := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a >= 0x8000 {
				out.SetColorIndex(x-bounds.Min.X, y-bounds.Min.Y, 1)
				filled++
			}
		}
	}
	if filled == 0 {
		return nil, errors.New("sprite is fully transparent")
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
