package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/rand"
	"encoding/hex"
	"testing"
)

func decode(t *testing.T, value string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Packet vectors from RFC 3610 appendix A. They use an 8-octet MIC and a
// 13-octet nonce; Matter uses a 16-octet MIC with the same nonce width, so
// these pin the algorithm and TestMatterParameters covers Matter's sizes.
func TestRFC3610PacketVectors(t *testing.T) {
	key := decode(t, "C0C1C2C3C4C5C6C7C8C9CACBCCCDCECF")
	cases := []struct {
		name       string
		nonce      string
		additional string
		plaintext  string
		want       string
	}{
		{
			name:       "packet vector 1",
			nonce:      "00000003020100A0A1A2A3A4A5",
			additional: "0001020304050607",
			plaintext:  "08090A0B0C0D0E0F101112131415161718191A1B1C1D1E",
			want:       "588C979A61C663D2F066D0C2C0F989806D5F6B61DAC38417E8D12CFDF926E0",
		},
		{
			name:       "packet vector 2",
			nonce:      "00000004030201A0A1A2A3A4A5",
			additional: "0001020304050607",
			plaintext:  "08090A0B0C0D0E0F101112131415161718191A1B1C1D1E1F",
			want:       "72C91A36E135F8CF291CA894085C87E3CC15C439C9E43A3BA091D56E10400916",
		},
		{
			name:       "packet vector 3",
			nonce:      "00000005040302A0A1A2A3A4A5",
			additional: "0001020304050607",
			plaintext:  "08090A0B0C0D0E0F101112131415161718191A1B1C1D1E1F20",
			want:       "51B1E5F44A197D1DA46B0F8E2D282AE871E838BB64DA8596574ADAA76FBD9FB0C5",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			block, err := aes.NewCipher(key)
			if err != nil {
				t.Fatal(err)
			}
			aead, err := NewCCM(block, 8, 13)
			if err != nil {
				t.Fatal(err)
			}
			nonce := decode(t, testCase.nonce)
			additional := decode(t, testCase.additional)
			plaintext := decode(t, testCase.plaintext)
			want := decode(t, testCase.want)

			sealed := aead.Seal(nil, nonce, plaintext, additional)
			if !bytes.Equal(sealed, want) {
				t.Fatalf("Seal = %X\n want %X", sealed, want)
			}
			opened, err := aead.Open(nil, nonce, sealed, additional)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(opened, plaintext) {
				t.Fatalf("Open = %X, want %X", opened, plaintext)
			}
		})
	}
}

func matterAEAD(t *testing.T, key []byte) interface {
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
	NonceSize() int
	Overhead() int
} {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := NewCCM(block, TagSize, NonceSize)
	if err != nil {
		t.Fatal(err)
	}
	return aead
}

func TestMatterParameters(t *testing.T) {
	key := make([]byte, 16)
	nonce := make([]byte, NonceSize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	aead := matterAEAD(t, key)
	if aead.NonceSize() != 13 || aead.Overhead() != 16 {
		t.Fatalf("nonce %d overhead %d, want 13/16", aead.NonceSize(), aead.Overhead())
	}

	for _, size := range []int{0, 1, 15, 16, 17, 1024} {
		plaintext := make([]byte, size)
		if _, err := rand.Read(plaintext); err != nil {
			t.Fatal(err)
		}
		additional := []byte("matter message header")
		sealed := aead.Seal(nil, nonce, plaintext, additional)
		if len(sealed) != size+TagSize {
			t.Fatalf("sealed %d bytes for %d of plaintext", len(sealed), size)
		}
		opened, err := aead.Open(nil, nonce, sealed, additional)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if !bytes.Equal(opened, plaintext) {
			t.Fatalf("size %d did not round-trip", size)
		}
	}
}

func TestAuthenticationFailures(t *testing.T) {
	key := bytes.Repeat([]byte{0x2A}, 16)
	nonce := bytes.Repeat([]byte{0x07}, NonceSize)
	aead := matterAEAD(t, key)
	plaintext := []byte("commissioning payload")
	additional := []byte("header")
	sealed := aead.Seal(nil, nonce, plaintext, additional)

	corruptions := map[string]func() ([]byte, []byte, []byte){
		"flipped ciphertext bit": func() ([]byte, []byte, []byte) {
			corrupted := bytes.Clone(sealed)
			corrupted[0] ^= 0x01
			return corrupted, nonce, additional
		},
		"flipped MIC bit": func() ([]byte, []byte, []byte) {
			corrupted := bytes.Clone(sealed)
			corrupted[len(corrupted)-1] ^= 0x80
			return corrupted, nonce, additional
		},
		"modified additional data": func() ([]byte, []byte, []byte) {
			return sealed, nonce, []byte("headeR")
		},
		"wrong nonce": func() ([]byte, []byte, []byte) {
			other := bytes.Clone(nonce)
			other[12] ^= 0xFF
			return sealed, other, additional
		},
		"truncated below the MIC": func() ([]byte, []byte, []byte) {
			return sealed[:TagSize-1], nonce, additional
		},
	}
	for name, corrupt := range corruptions {
		t.Run(name, func(t *testing.T) {
			ciphertext, useNonce, useAdditional := corrupt()
			if opened, err := aead.Open(nil, useNonce, ciphertext, useAdditional); err == nil {
				t.Fatalf("corrupted message opened as %q", opened)
			}
		})
	}
}

// Additional data is optional; the CCM flags byte differs when it is absent.
func TestWithoutAdditionalData(t *testing.T) {
	aead := matterAEAD(t, bytes.Repeat([]byte{0x11}, 16))
	nonce := bytes.Repeat([]byte{0x22}, NonceSize)
	plaintext := []byte("no header")

	sealed := aead.Seal(nil, nonce, plaintext, nil)
	opened, err := aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatal("round trip without additional data failed")
	}
	if _, err := aead.Open(nil, nonce, sealed, []byte("x")); err == nil {
		t.Fatal("adding additional data must invalidate the MIC")
	}
}

// The two-octet length prefix switches encoding above 0xFF00 bytes of
// additional data.
func TestLargeAdditionalData(t *testing.T) {
	aead := matterAEAD(t, bytes.Repeat([]byte{0x33}, 16))
	nonce := bytes.Repeat([]byte{0x44}, NonceSize)
	for _, size := range []int{0xFEFF, 0xFF00, 0x10000} {
		additional := bytes.Repeat([]byte{0x55}, size)
		sealed := aead.Seal(nil, nonce, []byte("payload"), additional)
		if _, err := aead.Open(nil, nonce, sealed, additional); err != nil {
			t.Fatalf("additional data of %d bytes: %v", size, err)
		}
	}
}

func TestInPlaceSeal(t *testing.T) {
	aead := matterAEAD(t, bytes.Repeat([]byte{0x66}, 16))
	nonce := bytes.Repeat([]byte{0x77}, NonceSize)
	plaintext := []byte("reuse my storage")
	expected := aead.Seal(nil, nonce, plaintext, nil)

	buffer := make([]byte, 0, len(plaintext)+TagSize)
	buffer = append(buffer, plaintext...)
	sealed := aead.Seal(buffer[:0], nonce, buffer, nil)
	if !bytes.Equal(sealed, expected) {
		t.Fatalf("in-place Seal = %X, want %X", sealed, expected)
	}
}

func TestRejectedParameters(t *testing.T) {
	block, err := aes.NewCipher(make([]byte, 16))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ tagSize, nonceSize int }{
		{15, 13}, // odd tag
		{2, 13},  // tag too short
		{18, 13}, // tag too long
		{16, 6},  // nonce too short
		{16, 14}, // nonce too long
	}
	for _, testCase := range cases {
		if _, err := NewCCM(block, testCase.tagSize, testCase.nonceSize); err == nil {
			t.Fatalf("tag %d nonce %d was accepted", testCase.tagSize, testCase.nonceSize)
		}
	}
}
