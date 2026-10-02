//! SHA-1 is hier uitsluitend de door Matter vereiste 20-byte key identifier.
pub(crate) fn key_id(public: &[u8; 65]) -> [u8; 20] {
    let mut padded = [0u8; 128];
    padded[..65].copy_from_slice(public);
    padded[65] = 128;
    padded[120..].copy_from_slice(&520u64.to_be_bytes());
    let mut h = [
        0x67452301u32,
        0xefcdab89,
        0x98badcfe,
        0x10325476,
        0xc3d2e1f0,
    ];
    for block in padded.chunks_exact(64) {
        let mut w = [0u32; 80];
        for (i, b) in block.chunks_exact(4).enumerate() {
            w[i] = u32::from_be_bytes([b[0], b[1], b[2], b[3]]);
        }
        for i in 16..80 {
            w[i] = (w[i - 3] ^ w[i - 8] ^ w[i - 14] ^ w[i - 16]).rotate_left(1);
        }
        let [mut a, mut b, mut c, mut d, mut e] = h;
        for (i, w) in w.iter().enumerate() {
            let (f, k) = match i {
                0..=19 => ((b & c) | (!b & d), 0x5a827999u32),
                20..=39 => (b ^ c ^ d, 0x6ed9eba1),
                40..=59 => ((b & c) | (b & d) | (c & d), 0x8f1bbcdc),
                _ => (b ^ c ^ d, 0xca62c1d6),
            };
            let t = a
                .rotate_left(5)
                .wrapping_add(f)
                .wrapping_add(e)
                .wrapping_add(k)
                .wrapping_add(*w);
            e = d;
            d = c;
            c = b.rotate_left(30);
            b = a;
            a = t;
        }
        for (h, v) in h.iter_mut().zip([a, b, c, d, e]) {
            *h = h.wrapping_add(v);
        }
    }
    let mut digest = [0; 20];
    for (out, v) in digest.chunks_exact_mut(4).zip(h) {
        out.copy_from_slice(&v.to_be_bytes());
    }
    digest
}
