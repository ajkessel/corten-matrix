// corten-matrix - A Matrix-iMessage puppeting bridge.
//
// Tests for re-encode-insensitive avatar identity.

package connector

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// testImage builds a small deterministic gradient with one distinguishing pixel,
// so two images can be made pixel-identical or pixel-different on demand.
func testImage(w, h int, tweak color.RGBA) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 3), G: uint8(y * 3), B: 0x40, A: 0xff})
		}
	}
	img.Set(0, 0, tweak)
	return img
}

func encodeJPEG(t *testing.T, img image.Image, quality int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

// TestAvatarContentHashIgnoresReEncoding is the regression test for the avatar
// churn: the SAME picture in a byte-different container must keep one identity,
// or the framework re-uploads it and PUTs the ghost's avatar_url, fanning a
// member event into every room the ghost shares. (Observed upstream: two
// pixel-identical 96x96 JPEGs of one contact photo differing in 1951 of 3658
// bytes, 15 minutes apart.) Trailing bytes after the end-of-image marker are
// the reproducible stand-in for that class of difference: decoders ignore them,
// so the pixels are identical by construction while the bytes are not.
func TestAvatarContentHashIgnoresReEncoding(t *testing.T) {
	img := testImage(32, 32, color.RGBA{R: 1, G: 2, B: 3, A: 0xff})
	original := encodeJPEG(t, img, 90)
	padded := append(append([]byte(nil), original...), 0xde, 0xad, 0xbe, 0xef)
	if bytes.Equal(original, padded) {
		t.Fatal("padded copy must differ in bytes")
	}

	if got, want := avatarContentHash(padded), avatarContentHash(original); got != want {
		t.Errorf("byte-different but pixel-identical JPEG hashed as %q, want %q — this is the churn", got, want)
	}

	// Same property for PNG, and the raw-byte hash must NOT be what identity is
	// keyed on (that is what made the churn possible in the first place).
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	pngOriginal := buf.Bytes()
	pngPadded := append(append([]byte(nil), pngOriginal...), 0x00, 0x01)
	if got, want := avatarContentHash(pngPadded), avatarContentHash(pngOriginal); got != want {
		t.Errorf("byte-different but pixel-identical PNG hashed as %q, want %q", got, want)
	}
	byteSum := sha256.Sum256(pngOriginal)
	if avatarContentHash(pngOriginal) == hex.EncodeToString(byteSum[:8]) {
		t.Error("identity is still keyed on raw bytes for a decodable image")
	}
}

// TestAvatarContentHashPixelIdentical pins the core property with two different
// containers holding the same pixels: PNG and a lossless-source RGBA image
// must produce the same identity, while a one-pixel change must not.
func TestAvatarContentHashPixelIdentical(t *testing.T) {
	img := testImage(16, 16, color.RGBA{R: 10, G: 20, B: 30, A: 0xff})

	var pngA, pngB bytes.Buffer
	if err := png.Encode(&pngA, img); err != nil {
		t.Fatal(err)
	}
	// Re-encode from a decoded copy: different byte stream is not guaranteed,
	// but the pixels are identical either way.
	decoded, err := png.Decode(bytes.NewReader(pngA.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(&pngB, decoded); err != nil {
		t.Fatal(err)
	}
	if got, want := avatarContentHash(pngB.Bytes()), avatarContentHash(pngA.Bytes()); got != want {
		t.Errorf("same pixels, different encode = %q, want %q", got, want)
	}

	changed := testImage(16, 16, color.RGBA{R: 11, G: 20, B: 30, A: 0xff})
	var pngC bytes.Buffer
	if err := png.Encode(&pngC, changed); err != nil {
		t.Fatal(err)
	}
	if avatarContentHash(pngC.Bytes()) == avatarContentHash(pngA.Bytes()) {
		t.Error("a one-pixel change must produce a different avatar identity")
	}
}

// TestAvatarContentHashDimensionsMatter: a crop or resize is a real change even
// when the surviving pixels are the same.
func TestAvatarContentHashDimensionsMatter(t *testing.T) {
	var small, large bytes.Buffer
	if err := png.Encode(&small, testImage(16, 16, color.RGBA{A: 0xff})); err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(&large, testImage(16, 24, color.RGBA{A: 0xff})); err != nil {
		t.Fatal(err)
	}
	if avatarContentHash(small.Bytes()) == avatarContentHash(large.Bytes()) {
		t.Error("different dimensions must produce different avatar identities")
	}
}

// TestAvatarContentHashUndecodable: anything we can't decode falls back to the
// raw-byte hash rather than collapsing to a single shared identity.
func TestAvatarContentHashUndecodable(t *testing.T) {
	a := avatarContentHash([]byte("<svg xmlns=\"http://www.w3.org/2000/svg\"/>"))
	b := avatarContentHash([]byte("<svg xmlns=\"http://www.w3.org/2000/svg\"><rect/></svg>"))
	if a == "" || b == "" {
		t.Fatal("undecodable data must still get an identity")
	}
	if a == b {
		t.Error("different undecodable data must not share one identity")
	}
	if avatarContentHash(nil) != "" {
		t.Error("empty avatar must have empty identity")
	}
}

// TestAvatarContentHashCacheIsBounded: the memo is keyed on the RAW BYTE hash,
// which is exactly what churns in the bug this file addresses — a server that
// re-encodes the same photo mints a new key on every fetch. Without a cap the
// map grows for the life of the process.
func TestAvatarContentHashCacheIsBounded(t *testing.T) {
	avatarContentHashCache.Range(func(k, _ any) bool {
		avatarContentHashCache.Delete(k)
		return true
	})
	avatarContentHashCount.Store(0)

	// Feed distinct undecodable blobs: each is a fresh byte key, standing in for
	// a fresh re-encode of one contact photo.
	for i := 0; i < avatarContentHashCacheMax+50; i++ {
		avatarContentHash([]byte(fmt.Sprintf("not-an-image-%d", i)))
	}

	entries := 0
	avatarContentHashCache.Range(func(_, _ any) bool {
		entries++
		return true
	})
	if entries > avatarContentHashCacheMax {
		t.Errorf("memo holds %d entries, want at most %d", entries, avatarContentHashCacheMax)
	}
	if entries == 0 {
		t.Error("memo emptied itself entirely — the cap should drop and refill, not disable caching")
	}
}
