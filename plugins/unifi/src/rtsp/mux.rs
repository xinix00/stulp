use super::{Codec, Info, bytes, codec};
use alloc::{string::String, vec::Vec};
use stulp_sdk::{Error, Result};
struct Boxes(Vec<u8>);
impl Boxes {
    fn data(&mut self, b: &[u8]) -> Result {
        bytes(&mut self.0, b)
    }
    fn zero(&mut self, n: usize) -> Result {
        for _ in 0..n {
            self.data(&[0])?;
        }
        Ok(())
    }
    fn u16(&mut self, n: u16) -> Result {
        self.data(&n.to_be_bytes())
    }
    fn u32(&mut self, n: u32) -> Result {
        self.data(&n.to_be_bytes())
    }
    fn u64(&mut self, n: u64) -> Result {
        self.data(&n.to_be_bytes())
    }
    fn boxed(&mut self, name: &[u8; 4], f: impl FnOnce(&mut Self) -> Result) -> Result {
        let start = self.0.len();
        self.u32(0)?;
        self.data(name)?;
        f(self)?;
        let len =
            u32::try_from(self.0.len() - start).map_err(|_| Error::Invalid("MP4 box too large"))?;
        self.0[start..start + 4].copy_from_slice(&len.to_be_bytes());
        Ok(())
    }
    fn matrix(&mut self) -> Result {
        for n in [0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000] {
            self.u32(n)?;
        }
        Ok(())
    }
}
/// Eén camera bezit één muxer; fragments beginnen op tijdstip nul en lopen door bij RTP-wrap.
pub struct Muxer {
    codec: Codec,
    header: Vec<u8>,
    mime: String,
    sequence: u32,
    last: Option<u32>,
    timeline: u64,
}
impl Muxer {
    /// Initialisatiesegment uit SPS/PPS van de camera.
    pub fn h264(sps: &[u8], pps: &[u8]) -> Result<Self> {
        Self::new(Info::h264(sps)?, sps, pps)
    }
    /// Initialisatiesegment zodra de eerste AV1 sequence header binnen is.
    pub fn av1(sequence: &[u8]) -> Result<Self> {
        Self::new(Info::av1(sequence)?, sequence, &[])
    }
    fn new(info: Info, first: &[u8], second: &[u8]) -> Result<Self> {
        let config = codec::config(&info, first, second)?;
        Ok(Self {
            codec: info.codec,
            header: header(&info, &config)?,
            mime: info.mime()?,
            sequence: 0,
            last: None,
            timeline: 0,
        })
    }
    /// ftyp + moov voor een nieuwe kijker.
    pub fn header(&self) -> &[u8] {
        &self.header
    }
    /// MediaSource content type met codec.
    pub fn mime(&self) -> &str {
        &self.mime
    }
    /// Alleen een zelfstandig decodeerbaar frame mag een nieuwe kijker starten.
    pub fn keyframe(&self, unit: &[Vec<u8>]) -> bool {
        codec::keyframe(self.codec, unit)
    }
    /// Verpakt een complete toegangseenheid. Teruglopende timestamps worden geweigerd.
    pub fn fragment(&mut self, unit: &[Vec<u8>], timestamp: u32) -> Result<Vec<u8>> {
        let sample = if self.codec == Codec::Av1 {
            codec::av1_sample(unit)?
        } else {
            let mut out = Vec::new();
            for nal in unit.iter().filter(|n| !n.is_empty()) {
                let n = u32::try_from(nal.len()).map_err(|_| Error::Invalid("NAL too large"))?;
                bytes(&mut out, &n.to_be_bytes())?;
                bytes(&mut out, nal)?;
            }
            out
        };
        if sample.is_empty() {
            return Ok(Vec::new());
        }
        let delta = self.last.map(|last| timestamp.wrapping_sub(last));
        if delta.is_some_and(|d| d >= 1 << 31) {
            return Err(Error::Invalid("camera timestamps moved backwards"));
        }
        let timeline = self
            .timeline
            .checked_add(u64::from(delta.unwrap_or(0)))
            .ok_or(Error::Invalid("camera timeline overflow"))?;
        let duration = delta.filter(|d| *d > 0 && *d < 900000).unwrap_or(3600);
        let seq = self
            .sequence
            .checked_add(1)
            .ok_or(Error::Invalid("MP4 sequence exhausted"))?;
        let mut b = Boxes(Vec::new());
        let mut offset_at = 0;
        b.boxed(b"moof", |b| {
            b.boxed(b"mfhd", |b| {
                b.u32(0)?;
                b.u32(seq)
            })?;
            b.boxed(b"traf", |b| {
                b.boxed(b"tfhd", |b| {
                    b.u32(0x020000)?;
                    b.u32(1)
                })?;
                b.boxed(b"tfdt", |b| {
                    b.u32(0x01000000)?;
                    b.u64(timeline)
                })?;
                b.boxed(b"trun", |b| {
                    b.u32(0x701)?;
                    b.u32(1)?;
                    offset_at = b.0.len();
                    b.u32(0)?;
                    b.u32(duration)?;
                    b.u32(sample.len() as u32)?;
                    b.u32(if self.keyframe(unit) {
                        0x02000000
                    } else {
                        0x01010000
                    })
                })
            })
        })?;
        let offset =
            u32::try_from(b.0.len() + 8).map_err(|_| Error::Invalid("MP4 offset overflow"))?;
        b.0.get_mut(offset_at..offset_at + 4)
            .ok_or(Error::Invalid("MP4 offset missing"))?
            .copy_from_slice(&offset.to_be_bytes());
        b.boxed(b"mdat", |b| b.data(&sample))?;
        self.sequence = seq;
        self.last = Some(timestamp);
        self.timeline = timeline;
        Ok(b.0)
    }
}
fn header(info: &Info, config: &[u8]) -> Result<Vec<u8>> {
    let mut b = Boxes(Vec::new());
    b.boxed(b"ftyp", |b| {
        b.data(b"isom")?;
        b.u32(512)?;
        b.data(b"isomiso2avc1mp41")
    })?;
    b.boxed(b"moov", |b| {
        b.boxed(b"mvhd", |b| {
            b.zero(12)?;
            b.u32(90000)?;
            b.u32(0)?;
            b.u32(0x10000)?;
            b.u16(0x100)?;
            b.zero(10)?;
            b.matrix()?;
            b.zero(24)?;
            b.u32(2)
        })?;
        b.boxed(b"trak", |b| {
            b.boxed(b"tkhd", |b| {
                b.u32(3)?;
                b.zero(8)?;
                b.u32(1)?;
                b.zero(24)?;
                b.matrix()?;
                b.u32(u32::from(info.width) << 16)?;
                b.u32(u32::from(info.height) << 16)
            })?;
            b.boxed(b"mdia", |b| {
                b.boxed(b"mdhd", |b| {
                    b.zero(12)?;
                    b.u32(90000)?;
                    b.u32(0)?;
                    b.u16(0x55c4)?;
                    b.u16(0)
                })?;
                b.boxed(b"hdlr", |b| {
                    b.zero(8)?;
                    b.data(b"vide")?;
                    b.zero(12)?;
                    b.data(b"VideoHandler\0")
                })?;
                b.boxed(b"minf", |b| {
                    b.boxed(b"vmhd", |b| {
                        b.u32(1)?;
                        b.zero(8)
                    })?;
                    b.boxed(b"dinf", |b| {
                        b.boxed(b"dref", |b| {
                            b.u32(0)?;
                            b.u32(1)?;
                            b.boxed(b"url ", |b| b.u32(1))
                        })
                    })?;
                    b.boxed(b"stbl", |b| {
                        b.boxed(b"stsd", |b| {
                            b.u32(0)?;
                            b.u32(1)?;
                            b.boxed(
                                if info.codec == Codec::H264 {
                                    b"avc1"
                                } else {
                                    b"av01"
                                },
                                |b| {
                                    b.zero(6)?;
                                    b.u16(1)?;
                                    b.zero(16)?;
                                    b.u16(info.width)?;
                                    b.u16(info.height)?;
                                    b.u32(0x00480000)?;
                                    b.u32(0x00480000)?;
                                    b.u32(0)?;
                                    b.u16(1)?;
                                    b.zero(32)?;
                                    b.u16(0x18)?;
                                    b.u16(0xffff)?;
                                    b.boxed(
                                        if info.codec == Codec::H264 {
                                            b"avcC"
                                        } else {
                                            b"av1C"
                                        },
                                        |b| b.data(config),
                                    )
                                },
                            )
                        })?;
                        for name in [b"stts", b"stsc", b"stsz", b"stco"] {
                            b.boxed(name, |b| b.zero(if name == b"stsz" { 12 } else { 8 }))?;
                        }
                        Ok(())
                    })
                })
            })
        })?;
        b.boxed(b"mvex", |b| {
            b.boxed(b"trex", |b| {
                b.u32(0)?;
                b.u32(1)?;
                b.u32(1)?;
                b.zero(12)
            })
        })
    })?;
    Ok(b.0)
}
