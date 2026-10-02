//! ZIP-directory en streaming bestandsinhoud; nooit een uitgepakte bundel in RAM.
use super::inflate::{Crc32, Deflate};
use std::io::{self, Read, Seek, SeekFrom, Write};

pub(super) const MAX_FILES: usize = 100_000;
pub(super) const MAX_BYTES: u64 = 2 << 30;
fn bad(message: &'static str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message)
}
fn le16(b: &[u8], at: usize) -> io::Result<u16> {
    Ok(u16::from_le_bytes(
        b.get(at..at + 2)
            .ok_or_else(|| bad("truncated ZIP field"))?
            .try_into()
            .map_err(|_| bad("ZIP field"))?,
    ))
}
fn le32(b: &[u8], at: usize) -> io::Result<u32> {
    Ok(u32::from_le_bytes(
        b.get(at..at + 4)
            .ok_or_else(|| bad("truncated ZIP field"))?
            .try_into()
            .map_err(|_| bad("ZIP field"))?,
    ))
}
fn le64(b: &[u8], at: usize) -> io::Result<u64> {
    Ok(u64::from_le_bytes(
        b.get(at..at + 8)
            .ok_or_else(|| bad("truncated ZIP field"))?
            .try_into()
            .map_err(|_| bad("ZIP field"))?,
    ))
}
fn buffer(len: usize) -> io::Result<Vec<u8>> {
    let mut out = Vec::new();
    out.try_reserve_exact(len).map_err(io::Error::other)?;
    out.resize(len, 0);
    Ok(out)
}
pub(super) struct Entry {
    pub name: String,
    pub mode: u32,
    pub size: u64,
    method: u16,
    flags: u16,
    compressed: u64,
    crc: u32,
    offset: u64,
    end: u64,
}
impl Entry {
    pub(super) fn directory(&self) -> bool {
        self.mode & 0o170000 == 0o040000 || self.name.ends_with('/')
    }
    pub(super) fn copy(
        &self,
        src: &mut (impl Read + Seek),
        output: &mut impl Write,
    ) -> io::Result<()> {
        src.seek(SeekFrom::Start(self.offset))?;
        let mut header = [0; 30];
        src.read_exact(&mut header)?;
        if le32(&header, 0)? != 0x04034b50
            || le16(&header, 8)? != self.method
            || le16(&header, 6)? != self.flags
        {
            return Err(bad("ZIP local header disagrees with directory"));
        }
        let name_len = usize::from(le16(&header, 26)?);
        let extra = u64::from(le16(&header, 28)?);
        let mut name = buffer(name_len)?;
        src.read_exact(&mut name)?;
        if name != self.name.as_bytes() {
            return Err(bad("ZIP local name disagrees with directory"));
        }
        let start = self
            .offset
            .checked_add(30 + name_len as u64 + extra)
            .ok_or_else(|| bad("ZIP offset overflow"))?;
        if start
            .checked_add(self.compressed)
            .is_none_or(|n| n > self.end)
        {
            return Err(bad("ZIP payload overlaps directory"));
        }
        src.seek(SeekFrom::Start(start))?;
        let raw = src.take(self.compressed);
        let (size, crc) = match self.method {
            0 => copy(raw, output, self.size)?,
            8 => copy(Deflate::new(raw)?, output, self.size)?,
            _ => return Err(bad("unsupported ZIP compression")),
        };
        if size != self.size || crc != self.crc {
            return Err(bad("ZIP length or checksum mismatch"));
        }
        Ok(())
    }
    pub(super) fn bytes(&self, src: &mut (impl Read + Seek), limit: usize) -> io::Result<Vec<u8>> {
        if self.directory() || self.size > limit as u64 {
            return Err(bad("ZIP entry exceeds document limit"));
        }
        let mut bytes = Vec::new();
        bytes
            .try_reserve_exact(self.size as usize)
            .map_err(io::Error::other)?;
        self.copy(src, &mut bytes)?;
        Ok(bytes)
    }
}
fn copy(mut src: impl Read, output: &mut impl Write, max: u64) -> io::Result<(u64, u32)> {
    let mut buffer = [0; 8192];
    let mut crc = Crc32::new();
    let mut count = 0_u64;
    loop {
        let n = match src.read(&mut buffer) {
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            value => value?,
        };
        if n == 0 {
            break;
        }
        count = count
            .checked_add(n as u64)
            .filter(|n| *n <= max)
            .ok_or_else(|| bad("ZIP expands beyond declared size"))?;
        let part = &buffer[..n];
        crc.update(part);
        output.write_all(part)?;
    }
    Ok((count, crc.sum()))
}
pub(super) fn directory(src: &mut (impl Read + Seek)) -> io::Result<Vec<Entry>> {
    let len = src.seek(SeekFrom::End(0))?;
    let mut tail = buffer(len.min(65557) as usize)?;
    src.seek(SeekFrom::End(-(tail.len() as i64)))?;
    src.read_exact(&mut tail)?;
    let at = (0..tail.len().saturating_sub(21))
        .rev()
        .find(|&i| {
            le32(&tail, i).ok() == Some(0x06054b50)
                && le16(&tail, i + 20).is_ok_and(|n| i + 22 + usize::from(n) == tail.len())
        })
        .ok_or_else(|| bad("missing ZIP directory"))?;
    let h = &tail[at..];
    if le16(h, 4)? != 0 || le16(h, 6)? != 0 || le16(h, 8)? != le16(h, 10)? {
        return Err(bad("multi-disk ZIP unsupported"));
    }
    let mut count = u64::from(le16(h, 10)?);
    let mut size = u64::from(le32(h, 12)?);
    let mut offset = u64::from(le32(h, 16)?);
    let mut directory_end = len - tail.len() as u64 + at as u64;
    if count == 65535 || size == u64::from(u32::MAX) || offset == u64::from(u32::MAX) {
        let loc_at = directory_end
            .checked_sub(20)
            .ok_or_else(|| bad("missing ZIP64 locator"))?;
        src.seek(SeekFrom::Start(loc_at))?;
        let mut locator = [0; 20];
        src.read_exact(&mut locator)?;
        if le32(&locator, 0)? != 0x07064b50 || le32(&locator, 4)? != 0 || le32(&locator, 16)? != 1 {
            return Err(bad("invalid ZIP64 locator"));
        }
        directory_end = le64(&locator, 8)?;
        src.seek(SeekFrom::Start(directory_end))?;
        let mut h = [0; 56];
        src.read_exact(&mut h)?;
        if directory_end + 56 > loc_at
            || le32(&h, 0)? != 0x06064b50
            || le32(&h, 16)? != 0
            || le32(&h, 20)? != 0
            || le64(&h, 24)? != le64(&h, 32)?
        {
            return Err(bad("invalid ZIP64 directory"));
        }
        count = le64(&h, 32)?;
        size = le64(&h, 40)?;
        offset = le64(&h, 48)?;
    }
    if count > MAX_FILES as u64
        || size > 64 << 20
        || offset
            .checked_add(size)
            .is_none_or(|end| end > directory_end)
    {
        return Err(bad("ZIP directory exceeds limits"));
    }
    src.seek(SeekFrom::Start(offset))?;
    let mut data = buffer(size as usize)?;
    src.read_exact(&mut data)?;
    let mut entries = Vec::new();
    entries
        .try_reserve_exact(count as usize)
        .map_err(io::Error::other)?;
    let mut at = 0;
    let mut expanded = 0_u64;
    for _ in 0..count {
        let h = data
            .get(at..at + 46)
            .ok_or_else(|| bad("truncated ZIP directory"))?;
        if le32(h, 0)? != 0x02014b50 || le16(h, 34)? != 0 {
            return Err(bad("invalid ZIP entry"));
        }
        let n = usize::from(le16(h, 28)?);
        let x = usize::from(le16(h, 30)?);
        let c = usize::from(le16(h, 32)?);
        let raw = data
            .get(at + 46..at + 46 + n)
            .ok_or_else(|| bad("truncated ZIP name"))?;
        let name = std::str::from_utf8(raw).map_err(|_| bad("ZIP name is not UTF-8"))?;
        let name = stulp_core::json::copy(name).map_err(io::Error::other)?;
        let mut entry = Entry {
            name,
            mode: if le16(h, 4)? >> 8 == 3 {
                le32(h, 38)? >> 16
            } else {
                0
            },
            size: u64::from(le32(h, 24)?),
            method: le16(h, 10)?,
            flags: le16(h, 8)?,
            compressed: u64::from(le32(h, 20)?),
            crc: le32(h, 16)?,
            offset: u64::from(le32(h, 42)?),
            end: offset,
        };
        let mut extra = data
            .get(at + 46 + n..at + 46 + n + x)
            .ok_or_else(|| bad("truncated ZIP extra"))?;
        while !extra.is_empty() {
            let kind = le16(extra, 0)?;
            let length = usize::from(le16(extra, 2)?);
            let block = extra
                .get(4..4 + length)
                .ok_or_else(|| bad("truncated ZIP extra block"))?;
            if kind == 1 {
                let mut k = 0;
                for value in [&mut entry.size, &mut entry.compressed, &mut entry.offset] {
                    if *value == u64::from(u32::MAX) {
                        *value = le64(block, k)?;
                        k += 8;
                    }
                }
            }
            extra = &extra[4 + length..];
        }
        if entry.flags & 0x41 != 0
            || !matches!(entry.method, 0 | 8)
            || !matches!(entry.mode & 0o170000, 0 | 0o100000 | 0o040000)
        {
            return Err(bad("encrypted, special or unsupported ZIP entry"));
        }
        expanded = expanded
            .checked_add(entry.size)
            .filter(|v| *v <= MAX_BYTES)
            .ok_or_else(|| bad("backup expands beyond 2 GiB"))?;
        entries.push(entry);
        at += 46 + n + x + c;
        if at > data.len() {
            return Err(bad("truncated ZIP comment"));
        }
    }
    if at != data.len() {
        return Err(bad("unexpected ZIP directory bytes"));
    }
    Ok(entries)
}
/// De schrijver gebruikt stored entries: direct streambaar en door Go leesbaar.
pub(super) struct Writer<W> {
    out: W,
    offset: u64,
    total: u64,
    entries: Vec<Entry>,
}
impl<W: Write> Writer<W> {
    pub(super) fn new(out: W) -> Self {
        Self {
            out,
            offset: 0,
            total: 0,
            entries: Vec::new(),
        }
    }
    pub(super) fn add(&mut self, name: &str, mode: u32, source: impl Read) -> io::Result<()> {
        if self.entries.len() >= MAX_FILES || name.len() > 65535 {
            return Err(bad("too many ZIP entries or name too long"));
        }
        self.entries.try_reserve(1).map_err(io::Error::other)?;
        let name = stulp_core::json::copy(name).map_err(io::Error::other)?;
        let mut header = [0_u8; 30];
        header[..4].copy_from_slice(&0x04034b50_u32.to_le_bytes());
        header[4..6].copy_from_slice(&20_u16.to_le_bytes());
        header[6..8].copy_from_slice(&0x808_u16.to_le_bytes());
        header[26..28].copy_from_slice(&(name.len() as u16).to_le_bytes());
        self.out.write_all(&header)?;
        self.out.write_all(name.as_bytes())?;
        let (size, crc) = copy(source, &mut self.out, MAX_BYTES - self.total)?;
        for n in [0x08074b50, crc, size as u32, size as u32] {
            self.out.write_all(&n.to_le_bytes())?;
        }
        let offset = self.offset;
        self.offset += 30 + name.len() as u64 + size + 16;
        self.total += size;
        self.entries.push(Entry {
            name,
            mode,
            method: 0,
            flags: 0x808,
            size,
            compressed: size,
            crc,
            offset,
            end: 0,
        });
        Ok(())
    }
    pub(super) fn finish(mut self) -> io::Result<W> {
        let start = self.offset;
        for e in &self.entries {
            let mut h = [0_u8; 46];
            h[..4].copy_from_slice(&0x02014b50_u32.to_le_bytes());
            h[4..6].copy_from_slice(&0x314_u16.to_le_bytes());
            h[6..8].copy_from_slice(&20_u16.to_le_bytes());
            h[8..10].copy_from_slice(&e.flags.to_le_bytes());
            h[16..20].copy_from_slice(&e.crc.to_le_bytes());
            h[20..24].copy_from_slice(&(e.size as u32).to_le_bytes());
            h[24..28].copy_from_slice(&(e.size as u32).to_le_bytes());
            h[28..30].copy_from_slice(&(e.name.len() as u16).to_le_bytes());
            h[38..42].copy_from_slice(&(e.mode << 16).to_le_bytes());
            h[42..46].copy_from_slice(
                &u32::try_from(e.offset)
                    .map_err(|_| bad("ZIP exceeds 32-bit offsets"))?
                    .to_le_bytes(),
            );
            self.out.write_all(&h)?;
            self.out.write_all(e.name.as_bytes())?;
            self.offset += 46 + e.name.len() as u64;
        }
        let size = self.offset - start;
        let count = self.entries.len() as u64;
        if count >= 65535 {
            let mut h = [0_u8; 56];
            h[..4].copy_from_slice(&0x06064b50_u32.to_le_bytes());
            h[4..12].copy_from_slice(&44_u64.to_le_bytes());
            h[12..14].copy_from_slice(&45_u16.to_le_bytes());
            h[14..16].copy_from_slice(&45_u16.to_le_bytes());
            h[24..32].copy_from_slice(&count.to_le_bytes());
            h[32..40].copy_from_slice(&count.to_le_bytes());
            h[40..48].copy_from_slice(&size.to_le_bytes());
            h[48..56].copy_from_slice(&start.to_le_bytes());
            self.out.write_all(&h)?;
            let mut locator = [0_u8; 20];
            locator[..4].copy_from_slice(&0x07064b50_u32.to_le_bytes());
            locator[8..16].copy_from_slice(&self.offset.to_le_bytes());
            locator[16..20].copy_from_slice(&1_u32.to_le_bytes());
            self.out.write_all(&locator)?;
        }
        let mut h = [0_u8; 22];
        h[..4].copy_from_slice(&0x06054b50_u32.to_le_bytes());
        let short = (count.min(65535) as u16).to_le_bytes();
        h[8..10].copy_from_slice(&short);
        h[10..12].copy_from_slice(&short);
        h[12..16].copy_from_slice(
            &u32::try_from(size)
                .map_err(|_| bad("ZIP directory too large"))?
                .to_le_bytes(),
        );
        h[16..20].copy_from_slice(
            &u32::try_from(start)
                .map_err(|_| bad("ZIP too large"))?
                .to_le_bytes(),
        );
        self.out.write_all(&h)?;
        self.out.flush()?;
        Ok(self.out)
    }
}
