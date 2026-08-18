// Package crypto holds the Matter primitives that Go's standard library does
// not provide: AES-CCM (the session cipher) and SPAKE2+ (the password
// authenticated key exchange behind PASE commissioning).
package crypto

import (
	"crypto/cipher"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
)

const blockSize = 16

// Matter's AEAD parameters (Core Specification, "Message Counter Encryption"):
// AES-CCM with a 128-bit key, a 16-octet MIC and a 13-octet nonce.
const (
	TagSize   = 16
	NonceSize = 13
)

type ccm struct {
	block     cipher.Block
	tagSize   int
	nonceSize int
}

// NewCCM returns AES-CCM as a cipher.AEAD (RFC 3610). Go's standard library
// ships GCM but not CCM, and Matter mandates CCM.
//
// As with the standard library's AEADs, dst may alias the input exactly
// (pass plaintext[:0] to reuse its storage) but must not overlap it
// partially.
func NewCCM(block cipher.Block, tagSize, nonceSize int) (cipher.AEAD, error) {
	if block.BlockSize() != blockSize {
		return nil, errors.New("matter/crypto: CCM requires a 128-bit block cipher")
	}
	if tagSize < 4 || tagSize > 16 || tagSize%2 != 0 {
		return nil, fmt.Errorf("matter/crypto: CCM tag size %d must be even and within [4,16]", tagSize)
	}
	if nonceSize < 7 || nonceSize > 13 {
		return nil, fmt.Errorf("matter/crypto: CCM nonce size %d must be within [7,13]", nonceSize)
	}
	return &ccm{block: block, tagSize: tagSize, nonceSize: nonceSize}, nil
}

func (c *ccm) NonceSize() int { return c.nonceSize }
func (c *ccm) Overhead() int  { return c.tagSize }

// maxPayload is bounded by the length field width L = 15 - nonceSize.
func (c *ccm) maxPayload() uint64 {
	length := 15 - c.nonceSize
	if length >= 8 {
		return ^uint64(0)
	}
	return 1<<(8*uint(length)) - 1
}

func (c *ccm) Seal(dst, nonce, plaintext, additionalData []byte) []byte {
	if len(nonce) != c.nonceSize {
		panic("matter/crypto: incorrect CCM nonce length")
	}
	if uint64(len(plaintext)) > c.maxPayload() {
		panic("matter/crypto: message too long for this CCM nonce length")
	}
	tag := c.tag(nonce, plaintext, additionalData)
	result, out := sliceForAppend(dst, len(plaintext)+c.tagSize)
	keystream0 := c.counterStream(nonce, out[:len(plaintext)], plaintext)
	for index := 0; index < c.tagSize; index++ {
		out[len(plaintext)+index] = tag[index] ^ keystream0[index]
	}
	return result
}

func (c *ccm) Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error) {
	if len(nonce) != c.nonceSize {
		return nil, errors.New("matter/crypto: incorrect CCM nonce length")
	}
	if len(ciphertext) < c.tagSize {
		return nil, errors.New("matter/crypto: CCM message is shorter than its MIC")
	}
	encrypted, receivedTag := ciphertext[:len(ciphertext)-c.tagSize], ciphertext[len(ciphertext)-c.tagSize:]
	if uint64(len(encrypted)) > c.maxPayload() {
		return nil, errors.New("matter/crypto: CCM message too long for this nonce length")
	}
	result, out := sliceForAppend(dst, len(encrypted))
	keystream0 := c.counterStream(nonce, out, encrypted)

	// The MIC covers the plaintext, so it can only be checked after
	// decrypting. Nothing is returned to the caller until it verifies.
	expected := c.tag(nonce, out, additionalData)
	for index := 0; index < c.tagSize; index++ {
		expected[index] ^= keystream0[index]
	}
	if subtle.ConstantTimeCompare(expected[:c.tagSize], receivedTag) != 1 {
		clear(out)
		return nil, errors.New("matter/crypto: CCM authentication failed")
	}
	return result, nil
}

// counterStream applies the CCM counter mode to source, writing to target,
// and returns S_0: the keystream block that masks the MIC.
func (c *ccm) counterStream(nonce []byte, target, source []byte) [blockSize]byte {
	var counter [blockSize]byte
	c.setCounter(&counter, nonce, 0)
	var keystream0 [blockSize]byte
	c.block.Encrypt(keystream0[:], counter[:])

	if len(source) > 0 {
		c.setCounter(&counter, nonce, 1)
		// Go's CTR increments the whole block, RFC 3610 only the trailing L
		// octets. maxPayload keeps the counter from ever carrying beyond
		// those octets, so the two are equivalent here.
		cipher.NewCTR(c.block, counter[:]).XORKeyStream(target, source)
	}
	return keystream0
}

func (c *ccm) setCounter(counter *[blockSize]byte, nonce []byte, value uint64) {
	length := 15 - c.nonceSize
	clear(counter[:])
	counter[0] = byte(length - 1)
	copy(counter[1:], nonce)
	for index := 0; index < length; index++ {
		counter[blockSize-1-index] = byte(value >> (8 * uint(index)))
	}
}

// tag computes the CBC-MAC over B_0, the length-prefixed additional data and
// the plaintext, each padded to a block boundary (RFC 3610 section 2.2).
func (c *ccm) tag(nonce, plaintext, additionalData []byte) [blockSize]byte {
	length := 15 - c.nonceSize
	mac := cbcMAC{block: c.block}

	var first [blockSize]byte
	first[0] = byte(length - 1)
	first[0] |= byte((c.tagSize-2)/2) << 3
	if len(additionalData) > 0 {
		first[0] |= 0x40
	}
	copy(first[1:], nonce)
	for index := 0; index < length; index++ {
		first[blockSize-1-index] = byte(uint64(len(plaintext)) >> (8 * uint(index)))
	}
	mac.write(first[:])

	if size := len(additionalData); size > 0 {
		var prefix [10]byte
		switch {
		case size < 0xFF00:
			binary.BigEndian.PutUint16(prefix[:2], uint16(size))
			mac.write(prefix[:2])
		case uint64(size) <= 0xFFFFFFFF:
			prefix[0], prefix[1] = 0xFF, 0xFE
			binary.BigEndian.PutUint32(prefix[2:6], uint32(size))
			mac.write(prefix[:6])
		default:
			prefix[0], prefix[1] = 0xFF, 0xFF
			binary.BigEndian.PutUint64(prefix[2:10], uint64(size))
			mac.write(prefix[:10])
		}
		mac.write(additionalData)
		mac.pad()
	}
	mac.write(plaintext)
	mac.pad()
	return mac.state
}

// cbcMAC is CBC-MAC with a zero IV over whole blocks.
type cbcMAC struct {
	block   cipher.Block
	state   [blockSize]byte
	partial [blockSize]byte
	used    int
}

func (m *cbcMAC) write(data []byte) {
	for len(data) > 0 {
		copied := copy(m.partial[m.used:], data)
		m.used += copied
		data = data[copied:]
		if m.used == blockSize {
			m.flush()
		}
	}
}

// pad completes a partial block with zeros, as CCM specifies.
func (m *cbcMAC) pad() {
	if m.used > 0 {
		clear(m.partial[m.used:])
		m.flush()
	}
}

func (m *cbcMAC) flush() {
	subtle.XORBytes(m.state[:], m.state[:], m.partial[:])
	m.block.Encrypt(m.state[:], m.state[:])
	m.used = 0
}

func sliceForAppend(in []byte, n int) (head, tail []byte) {
	if total := len(in) + n; cap(in) >= total {
		head = in[:total]
	} else {
		head = make([]byte, total)
		copy(head, in)
	}
	return head, head[len(in):]
}
