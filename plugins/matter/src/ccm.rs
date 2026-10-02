//! AES-CCM voor Matter; cleartext wordt pas na MIC-controle vrijgegeven.
use crate::copy;
use aes::{
    Aes128,
    cipher::{Block, BlockEncrypt, KeyInit},
};
use alloc::vec::Vec;
use stulp_sdk::{Error, Result};
use subtle::ConstantTimeEq;
use zeroize::Zeroize;
/// Matter-berichten gebruiken een 13-byte nonce; CASE gebruikt twaalf bytes.
pub struct Ccm {
    cipher: Aes128,
    tag: usize,
}
impl Ccm {
    /// Een afwijkende MIC-breedte is alleen nodig voor de RFC 3610-vectoren.
    pub fn new(key: &[u8; 16], tag: usize) -> Result<Self> {
        if !(4..=16).contains(&tag) || !tag.is_multiple_of(2) {
            return Err(Error::Invalid("invalid CCM tag size"));
        }
        Ok(Self {
            cipher: Aes128::new(key.into()),
            tag,
        })
    }
    fn nonce(nonce: &[u8]) -> Result {
        if !(7..=13).contains(&nonce.len()) {
            return Err(Error::Invalid("CCM nonce must be 7 to 13 bytes"));
        }
        Ok(())
    }
    fn encrypt(&self, block: &mut [u8; 16]) {
        let mut b = Block::<Aes128>::default();
        b.copy_from_slice(block);
        self.cipher.encrypt_block(&mut b);
        block.copy_from_slice(&b);
    }
    fn mac(&self, nonce: &[u8], plain: &[u8], aad: &[u8]) -> Result<[u8; 16]> {
        Self::nonce(nonce)?;
        if plain.len() > 65535 || aad.len() > 65535 {
            return Err(Error::Invalid("CCM input too long"));
        }
        let mut m = Mac {
            ccm: self,
            state: [0; 16],
            partial: [0; 16],
            used: 0,
        };
        let mut first = [0; 16];
        first[0] = (14 - nonce.len()) as u8
            | (((self.tag - 2) / 2) as u8) << 3
            | if aad.is_empty() { 0 } else { 64 };
        first[1..1 + nonce.len()].copy_from_slice(nonce);
        first[14..].copy_from_slice(&(plain.len() as u16).to_be_bytes());
        m.write(&first);
        if !aad.is_empty() {
            if aad.len() < 0xff00 {
                m.write(&(aad.len() as u16).to_be_bytes());
            } else {
                m.write(&[255, 254]);
                m.write(&(aad.len() as u32).to_be_bytes());
            }
            m.write(aad);
            m.pad();
        }
        m.write(plain);
        m.pad();
        Ok(m.state)
    }
    fn counter(&self, nonce: &[u8], value: u16) -> [u8; 16] {
        let mut block = [0; 16];
        block[0] = (14 - nonce.len()) as u8;
        block[1..1 + nonce.len()].copy_from_slice(nonce);
        block[14..].copy_from_slice(&value.to_be_bytes());
        self.encrypt(&mut block);
        block
    }
    fn crypt(&self, nonce: &[u8], buf: &mut [u8]) {
        for (i, block) in buf.chunks_mut(16).enumerate() {
            let stream = self.counter(nonce, (i + 1) as u16);
            for (b, k) in block.iter_mut().zip(stream) {
                *b ^= k;
            }
        }
    }
    /// Authenticeert de header en versleutelt de payload; nooit een impliciete nonce.
    pub fn seal(&self, nonce: &[u8], plain: &[u8], aad: &[u8]) -> Result<Vec<u8>> {
        let tag = self.mac(nonce, plain, aad)?;
        let mut out = copy(plain)?;
        self.crypt(nonce, &mut out);
        let mask = self.counter(nonce, 0);
        let mut mic = [0; 16];
        for i in 0..self.tag {
            mic[i] = tag[i] ^ mask[i];
        }
        // De wire-MIC valt buiten de 65535-byte CCM-plaintextgrens.
        out.try_reserve(self.tag)
            .map_err(|_| stulp_core::Error::Memory)?;
        out.extend_from_slice(&mic[..self.tag]);
        Ok(out)
    }
    /// Een onjuiste MIC wist de tijdelijke cleartext vóór terugkeer.
    pub fn open(&self, nonce: &[u8], wire: &[u8], aad: &[u8]) -> Result<Vec<u8>> {
        Self::nonce(nonce)?;
        let length = wire
            .len()
            .checked_sub(self.tag)
            .ok_or(Error::Invalid("CCM ciphertext shorter than MIC"))?;
        let mut out = copy(&wire[..length])?;
        self.crypt(nonce, &mut out);
        let mut tag = match self.mac(nonce, &out, aad) {
            Ok(t) => t,
            Err(e) => {
                out.zeroize();
                return Err(e);
            }
        };
        let mask = self.counter(nonce, 0);
        for i in 0..self.tag {
            tag[i] ^= mask[i];
        }
        if !bool::from(tag[..self.tag].ct_eq(&wire[length..])) {
            out.zeroize();
            return Err(Error::Invalid("CCM authentication failed"));
        }
        Ok(out)
    }
}
struct Mac<'a> {
    ccm: &'a Ccm,
    state: [u8; 16],
    partial: [u8; 16],
    used: usize,
}
impl Mac<'_> {
    fn write(&mut self, mut bytes: &[u8]) {
        while !bytes.is_empty() {
            let n = (16 - self.used).min(bytes.len());
            self.partial[self.used..self.used + n].copy_from_slice(&bytes[..n]);
            self.used += n;
            bytes = &bytes[n..];
            if self.used == 16 {
                self.flush();
            }
        }
    }
    fn flush(&mut self) {
        for i in 0..16 {
            self.state[i] ^= self.partial[i];
        }
        self.ccm.encrypt(&mut self.state);
        self.used = 0;
    }
    fn pad(&mut self) {
        if self.used > 0 {
            self.partial[self.used..].fill(0);
            self.flush();
        }
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    fn hex(s: &str) -> Vec<u8> {
        s.as_bytes()
            .chunks_exact(2)
            .map(|c| {
                let h = |b: u8| if b <= b'9' { b - b'0' } else { b - b'A' + 10 };
                h(c[0]) * 16 + h(c[1])
            })
            .collect()
    }
    #[test]
    fn rfc3610_vector_and_header_tampering() -> Result {
        let key = hex("C0C1C2C3C4C5C6C7C8C9CACBCCCDCECF");
        let key: &[u8; 16] = key
            .as_slice()
            .try_into()
            .map_err(|_| Error::Invalid("test key"))?;
        let nonce = hex("00000003020100A0A1A2A3A4A5");
        let nonce: &[u8; 13] = nonce
            .as_slice()
            .try_into()
            .map_err(|_| Error::Invalid("test nonce"))?;
        let a = hex("0001020304050607");
        let plain = hex("08090A0B0C0D0E0F101112131415161718191A1B1C1D1E");
        let c = Ccm::new(key, 8)?;
        let mut wire = c.seal(nonce, &plain, &a)?;
        assert_eq!(
            wire,
            hex("588C979A61C663D2F066D0C2C0F989806D5F6B61DAC38417E8D12CFDF926E0")
        );
        assert_eq!(c.open(nonce, &wire, &a)?, plain);
        wire[0] ^= 1;
        assert!(c.open(nonce, &wire, &a).is_err());
        wire[0] ^= 1;
        assert!(c.open(nonce, &wire, b"wrong").is_err());
        let c = Ccm::new(key, 16)?;
        for size in [0, 1, 15, 16, 17, 1024, 65535] {
            let mut p = Vec::new();
            p.resize(size, 19);
            assert_eq!(c.open(nonce, &c.seal(nonce, &p, &a)?, &a)?, p);
        }
        Ok(())
    }
}
