use super::{Assembler, Codec, Decoder, MAX_FRAME, Message, Muxer, Packet, Session, copy};
use alloc::{collections::VecDeque, string::String, vec::Vec};
use stulp_sdk::{Error, Result};
/// Werk voor de transport- of media-eigenaar. Iedere bytebuffer wordt verplaatst.
pub enum Output {
    /// RTSP-request naar dezelfde TCP/TLS-socket.
    Write(Vec<u8>),
    /// MP4-initialisatie, precies eenmaal per cameraverbinding.
    Header {
        /// MediaSource codec string.
        mime: String,
        /// ftyp + moov.
        bytes: Vec<u8>,
    },
    /// Volledige moof + mdat; kijkers mogen alleen bij een keyframe instappen.
    Frame {
        /// Zelfstandig decodeerbaar beeld.
        keyframe: bool,
        /// Containerbytes.
        bytes: Vec<u8>,
    },
}
impl Output {
    fn len(&self) -> usize {
        match self {
            Self::Write(b) | Self::Header { bytes: b, .. } | Self::Frame { bytes: b, .. } => {
                b.len()
            }
        }
    }
}
/// De hele RTSP-naar-fMP4-keten voor één camera, onafhankelijk van host/HopOS-I/O.
pub struct Camera {
    session: Session,
    decoder: Decoder,
    assembler: Option<Assembler>,
    muxer: Option<Muxer>,
    queue: VecDeque<Output>,
    queued: usize,
    setup_deadline: u64,
    read_deadline: u64,
    response_deadline: u64,
    keepalive: u64,
    started: bool,
    waiting_keyframe: bool,
    last_packet: Option<(u32, u16)>,
}
impl Camera {
    /// Timers gebruiken de monotone klok van de netwerkadapter.
    pub fn new(address: &str, now: u64) -> Result<Self> {
        Ok(Self {
            session: Session::new(address)?,
            decoder: Decoder::default(),
            assembler: None,
            muxer: None,
            queue: VecDeque::new(),
            queued: 0,
            setup_deadline: now.saturating_add(20000),
            read_deadline: now.saturating_add(20000),
            response_deadline: 0,
            keepalive: now.saturating_add(30000),
            started: false,
            waiting_keyframe: true,
            last_packet: None,
        })
    }
    /// Socketadres voor de transportadapter.
    pub fn target(&self) -> (&str, u16, bool) {
        (&self.session.host, self.session.port, self.session.tls)
    }
    /// Pas na succesvolle TCP/TLS-open aanroepen.
    pub fn opened(&mut self) -> Result {
        if self.started {
            return Err(Error::Invalid("camera socket opened twice"));
        }
        self.started = true;
        let b = self.session.start()?;
        self.push(Output::Write(b))
    }
    fn push(&mut self, output: Output) -> Result {
        let n = output.len();
        if self.queue.len() >= 8 || n > (MAX_FRAME + 65536).saturating_sub(self.queued) {
            return Err(Error::Invalid("camera output queue full"));
        }
        self.queue
            .try_reserve(1)
            .map_err(|_| stulp_core::Error::Memory)?;
        self.queue.push_back(output);
        self.queued += n;
        Ok(())
    }
    /// De caller neemt outputs over vóór nieuwe bytes te voeren; nooit onbegrensd bufferen.
    pub fn output(&mut self) -> Option<Output> {
        let out = self.queue.pop_front()?;
        self.queued -= out.len();
        Some(out)
    }
    /// Een willekeurig TCP-fragment. RTCP, keepalive en beeld mogen elkaar afwisselen.
    pub fn feed(&mut self, b: &[u8], now: u64) -> Result {
        if !self.started {
            return Err(Error::Invalid("camera data before socket open"));
        }
        self.decoder.feed(b)?;
        while let Some(message) = self.decoder.next_message()? {
            match message {
                Message::Response { .. } => {
                    if let Some(bytes) = self.session.response(message)? {
                        self.push(Output::Write(bytes))?;
                    }
                    if self.session.ready() && self.assembler.is_none() {
                        let media = self
                            .session
                            .media()
                            .ok_or(Error::Invalid("camera media missing"))?;
                        self.assembler = Some(Assembler::new(media.codec, media.payload));
                        if media.codec == Codec::H264 {
                            self.muxer = Some(Muxer::h264(&media.sps, &media.pps)?);
                            self.header()?;
                        }
                        self.keepalive = now.saturating_add(30000);
                        self.read_deadline = now.saturating_add(20000);
                    }
                    self.response_deadline = 0;
                }
                Message::Interleaved { channel: 0, bytes } => self.packet(&bytes, now)?,
                Message::Interleaved { .. } => (),
            }
        }
        Ok(())
    }
    fn header(&mut self) -> Result {
        let mux = self
            .muxer
            .as_ref()
            .ok_or(Error::Invalid("camera muxer missing"))?;
        let out = Output::Header {
            mime: stulp_core::json::copy(mux.mime())?,
            bytes: copy(mux.header())?,
        };
        self.push(out)
    }
    fn packet(&mut self, b: &[u8], now: u64) -> Result {
        if !self.session.ready() {
            return Err(Error::Invalid("camera sent RTP before PLAY"));
        }
        let p = Packet::parse(b)?;
        if self
            .session
            .media()
            .is_none_or(|m| m.payload != p.payload_type)
        {
            return Ok(());
        }
        if self
            .last_packet
            .is_some_and(|(source, seq)| source != p.source || p.sequence.wrapping_sub(seq) != 1)
        {
            self.waiting_keyframe = true;
        }
        self.last_packet = Some((p.source, p.sequence));
        self.read_deadline = now.saturating_add(20000);
        let assembler = self
            .assembler
            .as_mut()
            .ok_or(Error::Invalid("camera assembler missing"))?;
        let units = assembler.push(p)?;
        if self.muxer.is_none() && !assembler.sequence_header().is_empty() {
            self.muxer = Some(Muxer::av1(assembler.sequence_header())?);
            self.header()?;
        }
        for unit in units {
            let Some(mux) = &mut self.muxer else {
                continue;
            };
            let keyframe = mux.keyframe(&unit.parts);
            if self.waiting_keyframe && !keyframe {
                continue;
            }
            self.waiting_keyframe = false;
            let bytes = mux.fragment(&unit.parts, unit.timestamp)?;
            if !bytes.is_empty() {
                self.push(Output::Frame { keyframe, bytes })?;
            }
        }
        Ok(())
    }
    /// De caller blijft deze timer pollen terwijl er geen camerabytes komen.
    pub fn tick(&mut self, now: u64) -> Result {
        if (!self.session.ready() && now >= self.setup_deadline)
            || now >= self.read_deadline
            || (self.response_deadline != 0 && now >= self.response_deadline)
        {
            return Err(Error::Timeout);
        }
        if self.session.ready() && self.response_deadline == 0 && now >= self.keepalive {
            let b = self.session.keepalive()?;
            self.push(Output::Write(b))?;
            self.response_deadline = now.saturating_add(10000);
            self.keepalive = now.saturating_add(30000);
        }
        Ok(())
    }
}
