package imgguard

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/png"
	"testing"
)

// pngWithSize returns a 1x1 PNG whose IHDR claims w x h (a decompression-bomb header).
func pngWithSize(t *testing.T, w, h uint32) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	// signature(8) + length(4) + "IHDR"(4) + data(13) + crc(4)
	binary.BigEndian.PutUint32(b[16:], w)
	binary.BigEndian.PutUint32(b[20:], h)
	binary.BigEndian.PutUint32(b[29:], crc32.ChecksumIEEE(b[12:29]))
	return b
}

func TestRejectsHugeDimensionsBeforeDecoding(t *testing.T) {
	if _, _, err := Decode(pngWithSize(t, 20000, 20000)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("400 Mpixel header: err=%v, want ErrTooLarge", err)
	}
	if _, _, err := Config(pngWithSize(t, 6000, 6000)); err != nil {
		t.Fatalf("36 Mpixel header must pass: %v", err)
	}
	if _, _, err := Config(pngWithSize(t, 7000, 6000)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("42 Mpixel header must be rejected: %v", err)
	}
}

func TestDecodeSmallImage(t *testing.T) {
	var buf bytes.Buffer
	_ = png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8)))
	img, f, err := Decode(buf.Bytes())
	if err != nil || f != "png" || img.Bounds().Dx() != 8 {
		t.Fatalf("decode: %v %v", f, err)
	}
}
