//! Fallible bounded archive I/O without a native filesystem dependency.
use alloc::vec::Vec;
pub(super) use stulp_core::{Error, Result};
pub(super) trait Read {
    fn read(&mut self, out: &mut [u8]) -> Result<usize>;
    fn read_exact(&mut self, mut out: &mut [u8]) -> Result {
        while !out.is_empty() {
            let n = self.read(out)?;
            if n == 0 || n > out.len() {
                return Err(Error::Invalid("truncated archive"));
            }
            out = &mut out[n..];
        }
        Ok(())
    }
    fn take(self, remaining: u64) -> Take<Self>
    where
        Self: Sized,
    {
        Take {
            inner: self,
            remaining,
        }
    }
}
pub(super) struct Take<R> {
    inner: R,
    remaining: u64,
}
impl<R: Read> Read for Take<R> {
    fn read(&mut self, out: &mut [u8]) -> Result<usize> {
        let cap = out
            .len()
            .min(usize::try_from(self.remaining).unwrap_or(usize::MAX));
        let n = self.inner.read(&mut out[..cap])?;
        self.remaining = self.remaining.checked_sub(n as u64).ok_or(Error::Full)?;
        Ok(n)
    }
}
impl<R: Read + ?Sized> Read for &mut R {
    fn read(&mut self, out: &mut [u8]) -> Result<usize> {
        (**self).read(out)
    }
}
impl Read for &[u8] {
    fn read(&mut self, out: &mut [u8]) -> Result<usize> {
        let n = out.len().min(self.len());
        out[..n].copy_from_slice(&self[..n]);
        *self = &self[n..];
        Ok(n)
    }
}
pub(super) trait Write {
    fn write_all(&mut self, bytes: &[u8]) -> Result;
    fn flush(&mut self) -> Result {
        Ok(())
    }
}
impl Write for Vec<u8> {
    fn write_all(&mut self, bytes: &[u8]) -> Result {
        if bytes.len() > ((80usize << 20).saturating_sub(self.len())) {
            return Err(Error::Full);
        }
        self.try_reserve(bytes.len()).map_err(|_| Error::Memory)?;
        self.extend_from_slice(bytes);
        Ok(())
    }
}
impl<W: Write + ?Sized> Write for &mut W {
    fn write_all(&mut self, bytes: &[u8]) -> Result {
        (**self).write_all(bytes)
    }
    fn flush(&mut self) -> Result {
        (**self).flush()
    }
}
pub(super) enum SeekFrom {
    Start(u64),
    End(i64),
}
pub(super) trait Seek {
    fn seek(&mut self, at: SeekFrom) -> Result<u64>;
}
pub(super) struct Cursor<'a> {
    bytes: &'a [u8],
    at: usize,
}
impl<'a> Cursor<'a> {
    pub(super) fn new(bytes: &'a [u8]) -> Self {
        Self { bytes, at: 0 }
    }
}
impl Read for Cursor<'_> {
    fn read(&mut self, out: &mut [u8]) -> Result<usize> {
        let bytes = self.bytes.get(self.at..).ok_or(Error::Full)?;
        let n = out.len().min(bytes.len());
        out[..n].copy_from_slice(&bytes[..n]);
        self.at += n;
        Ok(n)
    }
}
impl Seek for Cursor<'_> {
    fn seek(&mut self, at: SeekFrom) -> Result<u64> {
        let n = match at {
            SeekFrom::Start(n) => n,
            SeekFrom::End(n) => (self.bytes.len() as u64)
                .checked_add_signed(n)
                .ok_or(Error::Full)?,
        };
        self.at = usize::try_from(n).map_err(|_| Error::Full)?;
        if self.at > self.bytes.len() {
            return Err(Error::Invalid("archive seek past end"));
        }
        Ok(n)
    }
}
