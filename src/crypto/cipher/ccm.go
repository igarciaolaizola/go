// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cipher

import (
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"math"
)

// ccm represents a Counter with CBC-MAC with a specific key.
type ccm struct {
	b Block
	M uint8
	L uint8
}

const ccmBlockSize = 16

// CCM is a block cipher in Counter with CBC-MAC mode.
// Providing authenticated encryption with associated data via the cipher.AEAD interface.
type CCM interface {
	AEAD
	// MaxLength returns the maxium length of plaintext in calls to Seal.
	// The maximum length of ciphertext in calls to Open is MaxLength()+Overhead().
	// The maximum length is related to CCM's `L` parameter (15-noncesize) and
	// is 1<<(8*L) - 1 (but also limited by the maxium size of an int).
	MaxLength() int
}

// NewCCM returns the given 128-bit block cipher wrapped in CCM.
// The tagsize must be an even integer between 4 and 16 inclusive
// and is used as CCM's `M` parameter.
// The noncesize must be an integer between 7 and 13 inclusive,
// 15-noncesize is used as CCM's `L` parameter.
func NewCCM(b Block, tagsize, noncesize int) (CCM, error) {
	if b.BlockSize() != ccmBlockSize {
		return nil, errors.New("cipher: NewCCM requires 128-bit block cipher")
	}
	if tagsize < 4 || tagsize > 16 || tagsize&1 != 0 {
		return nil, errors.New("cipher: tagsize must be 4, 6, 8, 10, 12, 14, or 16")
	}
	lensize := 15 - noncesize
	if lensize < 2 || lensize > 8 {
		return nil, errors.New("cipher: invalid noncesize")
	}
	c := &ccm{b: b, M: uint8(tagsize), L: uint8(lensize)}
	return c, nil
}

func (c *ccm) NonceSize() int { return 15 - int(c.L) }
func (c *ccm) Overhead() int  { return int(c.M) }
func (c *ccm) MaxLength() int { return maxlen(c.L, c.Overhead()) }

func maxlen(L uint8, tagsize int) int {
	max := (uint64(1) << (8 * L)) - 1
	if m64 := uint64(math.MaxInt64) - uint64(tagsize); L > 8 || max > m64 {
		max = m64 // The maximum lentgh on a 64bit arch
	}
	if max != uint64(int(max)) {
		return math.MaxInt32 - tagsize // We have only 32bit int's
	}
	return int(max)
}

// MaxNonceLength returns the maximum nonce length for a given plaintext length.
// A return value <= 0 indicates that plaintext length is too large for
// any nonce length.
func MaxNonceLength(pdatalen int) int {
	const tagsize = 16
	for L := 2; L <= 8; L++ {
		if maxlen(uint8(L), tagsize) >= pdatalen {
			return 15 - L
		}
	}
	return 0
}

func (c *ccm) cbcRound(mac, data []byte) {
	for i := 0; i < ccmBlockSize; i++ {
		mac[i] ^= data[i]
	}
	c.b.Encrypt(mac, mac)
}

func (c *ccm) cbcData(mac, data []byte) {
	for len(data) >= ccmBlockSize {
		c.cbcRound(mac, data[:ccmBlockSize])
		data = data[ccmBlockSize:]
	}
	if len(data) > 0 {
		var block [ccmBlockSize]byte
		copy(block[:], data)
		c.cbcRound(mac, block[:])
	}
}

func (c *ccm) tag(nonce, plaintext, adata []byte) ([]byte, error) {
	var mac [ccmBlockSize]byte

	if len(adata) > 0 {
		mac[0] |= 1 << 6
	}
	mac[0] |= (c.M - 2) << 2
	mac[0] |= c.L - 1
	if len(nonce) != c.NonceSize() {
		return nil, errors.New("cipher: Invalid nonce size")
	}
	if len(plaintext) > c.MaxLength() {
		return nil, errors.New("cipher: plaintext too large")
	}
	binary.BigEndian.PutUint64(mac[ccmBlockSize-8:], uint64(len(plaintext)))
	copy(mac[1:ccmBlockSize-c.L], nonce)
	c.b.Encrypt(mac[:], mac[:])

	var block [ccmBlockSize]byte
	if n := uint64(len(adata)); n > 0 {
		// First adata block includes adata length
		i := 2
		if n <= 0xfeff {
			binary.BigEndian.PutUint16(block[:i], uint16(n))
		} else {
			block[0] = 0xfe
			block[1] = 0xff
			if n < uint64(1<<32) {
				i = 2 + 4
				binary.BigEndian.PutUint32(block[2:i], uint32(n))
			} else {
				i = 2 + 8
				binary.BigEndian.PutUint64(block[2:i], uint64(n))
			}
		}
		i = copy(block[i:], adata)
		c.cbcRound(mac[:], block[:])
		c.cbcData(mac[:], adata[i:])
	}

	if len(plaintext) > 0 {
		c.cbcData(mac[:], plaintext)
	}

	return mac[:c.M], nil
}

// Seal encrypts and authenticates plaintext, authenticates the
// additional data and appends the result to dst, returning the updated
// slice. The nonce must be NonceSize() bytes long and unique for all
// time, for a given key.
// The plaintext must be no longer than MaxLength() bytes long.
//
// The plaintext and dst may alias exactly or not at all.
func (c *ccm) Seal(dst, nonce, plaintext, adata []byte) []byte {
	tag, err := c.tag(nonce, plaintext, adata)
	if err != nil {
		// The cipher.AEAD interface doesn't allow for an error return.
		panic(err)
	}

	var iv, s0 [ccmBlockSize]byte
	iv[0] = c.L - 1
	copy(iv[1:ccmBlockSize-c.L], nonce)
	c.b.Encrypt(s0[:], iv[:])
	for i := 0; i < int(c.M); i++ {
		tag[i] ^= s0[i]
	}
	iv[len(iv)-1] |= 1
	stream := NewCTR(c.b, iv[:])
	ret, out := sliceForAppend(dst, len(plaintext)+int(c.M))
	stream.XORKeyStream(out, plaintext)
	copy(out[len(plaintext):], tag)
	return ret
}

func (c *ccm) Open(dst, nonce, ciphertext, adata []byte) ([]byte, error) {
	if len(ciphertext) < int(c.M) {
		return nil, errors.New("cipher: ciphertext too short")
	}
	if len(ciphertext) > c.MaxLength()+c.Overhead() {
		return nil, errors.New("cipher: ciphertext too long")
	}

	tag := ciphertext[len(ciphertext)-int(c.M):]
	ciphertext = ciphertext[:len(ciphertext)-int(c.M)]

	var iv, s0 [ccmBlockSize]byte
	iv[0] = c.L - 1
	copy(iv[1:ccmBlockSize-c.L], nonce)
	c.b.Encrypt(s0[:], iv[:])
	for i := 0; i < int(c.M); i++ {
		tag[i] ^= s0[i]
	}
	iv[len(iv)-1] |= 1
	stream := NewCTR(c.b, iv[:])

	// Cannot decrypt directly to dst since we're not supposed to
	// reveal the plaintext to the caller if authentication fails.
	plaintext := make([]byte, len(ciphertext))
	stream.XORKeyStream(plaintext, ciphertext)
	expectedTag, err := c.tag(nonce, plaintext, adata)
	if err != nil {
		return nil, err
	}

	if subtle.ConstantTimeCompare(tag, expectedTag) != 1 {
		return nil, errors.New("cipher: tag missmatch")
	}
	return append(dst, plaintext...), nil
}
