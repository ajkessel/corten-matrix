// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// Stable identity for contact / shared-profile avatars.

package connector

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"sync"
	"sync/atomic"
)

// avatarContentHashCache memoizes decoded-pixel hashes keyed by the raw bytes'
// hash, so each distinct photo is decoded at most once per process. The address
// book is re-read every 15 minutes and every ghost is reconciled from it, so
// without this the same few hundred photos would be decoded on every cycle.
//
// Keyed on the BYTE hash, which is exactly what churns in the bug this file
// exists for: a server that re-encodes the same photo mints a new key on every
// fetch, so "one entry per distinct avatar" is not a bound. Entries are counted
// and the whole map is dropped past avatarContentHashCacheMax — a cold rebuild
// costs one decode per photo on the next cycle, which is what the memo saves in
// the first place, so the failure mode of the cap is mild.
//
// The count is deliberately not synchronized with the clear: two goroutines can
// clear concurrently and one Store(0) can land after the other's Add(1), so the
// count can drift low and the map briefly exceed the cap. This is a memo, not a
// correctness structure — a wrong entry is impossible (keys are content
// hashes), and the bound still holds in practice — so it isn't worth a mutex on
// a path that runs per ghost per reconcile.
var (
	avatarContentHashCache sync.Map // string (byte hash) -> string (content hash)
	avatarContentHashCount atomic.Int64
)

// avatarContentHashCacheMax caps the memo. A large address book holds a few
// hundred photos, so this is roughly an order of magnitude of headroom before
// a re-encoding server can grow it without bound.
const avatarContentHashCacheMax = 4096

// avatarContentHash returns a hex identity for an avatar that is derived from
// the image's DECODED PIXELS rather than its file bytes.
//
// Avatar IDs used to embed a hash of the raw bytes, which made the bridge
// treat a re-encode of the same picture as a brand-new avatar: the ID changed,
// so the framework re-uploaded the image, got a fresh MXC, and PUT the ghost's
// avatar_url — fanning a member event into every room that ghost shares and
// re-stamping every DM room avatar. Observed in the wild: the CardDAV server
// served two byte-different (1951 of 3658 bytes) but pixel-identical 96x96
// JPEGs of the same contact photo 15 minutes apart, and every ghost for that
// contact churned. Pixel content is what users actually see, so keying identity
// on it means a cosmetic re-encode is correctly a no-op.
//
// Re-keying every avatar_id on upgrade is silent for photos whose bytes are
// unchanged — bridgev2's Reupload returns the existing MXC when the byte hash
// matches, so prepareAvatar reports mxcChanged=false and no Matrix write
// happens. It is NOT silent for a photo the server happens to re-encode on the
// first post-upgrade fetch: that is a genuine byte change, so it uploads once
// and then settles, which is the whole point of the pixel keying.
//
// Falls back to the raw-byte hash when the data isn't a decodable image (SVG,
// a truncated download, an unknown format) — that keeps the previous behavior
// for anything we can't inspect rather than collapsing all of it to one ID.
func avatarContentHash(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	byteSum := sha256.Sum256(data)
	byteKey := hex.EncodeToString(byteSum[:8])
	if cached, ok := avatarContentHashCache.Load(byteKey); ok {
		return cached.(string)
	}

	result := byteKey
	if img, _, err := image.Decode(bytes.NewReader(data)); err == nil {
		result = pixelHash(img)
	}
	if avatarContentHashCount.Load() >= avatarContentHashCacheMax {
		avatarContentHashCache.Range(func(k, _ any) bool {
			avatarContentHashCache.Delete(k)
			return true
		})
		avatarContentHashCount.Store(0)
	}
	if _, loaded := avatarContentHashCache.LoadOrStore(byteKey, result); !loaded {
		avatarContentHashCount.Add(1)
	}
	return result
}

// pixelHash hashes an image's dimensions and every pixel's RGBA value. Uses the
// generic image.Image interface (not a type switch on the concrete buffer) so
// two encodings of the same picture hash equal even when they decode to
// different in-memory layouts — e.g. a JPEG decoding to YCbCr and a PNG of the
// same image decoding to RGBA.
func pixelHash(img image.Image) string {
	b := img.Bounds()
	h := sha256.New()
	var dims [8]byte
	binary.BigEndian.PutUint32(dims[0:4], uint32(b.Dx()))
	binary.BigEndian.PutUint32(dims[4:8], uint32(b.Dy()))
	h.Write(dims[:])
	var px [8]byte
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := img.At(x, y).RGBA()
			binary.BigEndian.PutUint16(px[0:2], uint16(r))
			binary.BigEndian.PutUint16(px[2:4], uint16(g))
			binary.BigEndian.PutUint16(px[4:6], uint16(bl))
			binary.BigEndian.PutUint16(px[6:8], uint16(a))
			h.Write(px[:])
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}
