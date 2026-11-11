// Package imgguard protects image decoding from decompression bombs: it reads the header
// first (cheap), rejects images with too many pixels, and bounds concurrent full decodes.
package imgguard

import (
	"bytes"
	"errors"
	"image"
)

// MaxPixels is the largest image (width*height) Weave will decode (~40 Mpixel).
const MaxPixels = 40_000_000

// ErrTooLarge is returned when an image exceeds MaxPixels.
var ErrTooLarge = errors.New("image dimensions are too large (max 40 megapixels)")

var sem = make(chan struct{}, 2)

// Config reads the image header and enforces MaxPixels. It returns the header config and the
// format name; err is ErrTooLarge for oversized images and the decoder's error otherwise.
func Config(data []byte) (image.Config, string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return cfg, format, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return cfg, format, errors.New("invalid image")
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return cfg, format, ErrTooLarge
	}
	return cfg, format, nil
}

// Decode checks the header, then fully decodes while holding one of a small number of
// decode slots so concurrent uploads cannot multiply memory use.
func Decode(data []byte) (image.Image, string, error) {
	if _, _, err := Config(data); err != nil {
		return nil, "", err
	}
	sem <- struct{}{}
	defer func() { <-sem }()
	return image.Decode(bytes.NewReader(data))
}
