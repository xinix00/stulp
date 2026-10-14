//! SHA-1 is hier uitsluitend de door Matter vereiste 20-byte key identifier.
pub(crate) fn key_id(public: &[u8; 65]) -> [u8; 20] {
    leancrypto::sha1::Sha1::digest(public)
}
