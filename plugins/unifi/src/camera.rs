//! Cameraverbindingen blijven eigendom van UniFi; alleen HTTP-uitvoer woont in de adapter.
use crate::{Unifi, events, rtsp};
use alloc::{string::String, vec::Vec};
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Client, Error, MediaCommand, MediaEvent, Result, StreamCommand, StreamEvent, Transport,
    util::join,
};
pub(super) struct Live {
    device: String,
    id: u64,
    camera: rtsp::Camera,
    token: String,
    url: String,
    mime: String,
}
impl Live {
    fn drain<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        while let Some(out) = self.camera.output() {
            match out {
                rtsp::Output::Write(bytes) => {
                    c.stream(StreamCommand::Write { id: self.id, bytes })?
                }
                rtsp::Output::Header { mime, bytes } => {
                    c.media(MediaCommand::Register {
                        id: self.id,
                        token: json::copy(&self.token)?,
                        mime: json::copy(&mime)?,
                        header: bytes,
                    })?;
                    self.mime = mime;
                }
                rtsp::Output::Frame { keyframe, bytes } => c.media(MediaCommand::Frame {
                    id: self.id,
                    keyframe,
                    bytes,
                })?,
            }
        }
        Ok(())
    }
    fn close<T: Transport>(&self, c: &mut Client<T>) -> Result {
        let result = c.stream(StreamCommand::Close { id: self.id });
        let media = c.media(MediaCommand::Close { id: self.id });
        result.and(media)
    }
}
impl Unifi {
    pub(super) fn close_cameras<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        device: Option<&str>,
    ) -> Result {
        let mut i = 0;
        while i < self.cameras.len() {
            if device.is_none_or(|d| d == self.cameras[i].device) {
                self.cameras.remove(i).close(c)?;
            } else {
                i += 1;
            }
        }
        Ok(())
    }
    pub(super) fn camera_io<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        while let Some(event) = c.poll_media() {
            let id = match event {
                MediaEvent::Idle(id) => id,
                MediaEvent::Failed(id, e) => {
                    self.error = stulp_sdk::message(&e)?;
                    id
                }
            };
            if let Some(i) = self.cameras.iter().position(|s| s.id == id) {
                self.cameras.remove(i).close(c)?;
            }
        }
        for _ in 0..32 {
            let Some(event) = c.poll_stream() else {
                break;
            };
            let id = events::id(&event);
            if let Some(s) = self.subscriptions.iter_mut().find(|s| s.id == id) {
                if let Err(e) = s.event(c, &self.config, event) {
                    s.failed(c, e)?;
                }
            } else if let Some(i) = self.cameras.iter().position(|s| s.id == id) {
                let live = &mut self.cameras[i];
                let result = match event {
                    StreamEvent::Opened(_) => live.camera.opened(),
                    StreamEvent::Data(_, bytes) => live.camera.feed(&bytes, c.now()),
                    StreamEvent::Closed(_, e) => Err(e),
                }
                .and_then(|()| live.drain(c));
                if let Err(e) = result {
                    self.error = stulp_sdk::message(&e)?;
                    self.cameras.remove(i).close(c)?;
                }
            }
        }
        let mut i = 0;
        while i < self.cameras.len() {
            let live = &mut self.cameras[i];
            let result = live.camera.tick(c.now()).and_then(|()| live.drain(c));
            if let Err(e) = result {
                self.error = stulp_sdk::message(&e)?;
                self.cameras.remove(i).close(c)?;
            } else {
                i += 1;
            }
        }
        Ok(())
    }
    pub(super) async fn video<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        p: &Value,
    ) -> Result<Value> {
        let device = json::text(p, "deviceId");
        let d = self.owned(device)?;
        if d.driver == "camera"
            && json::text(p, "slot") == "snapshot"
            && matches!(json::text(p, "kind"), "" | "image")
        {
            let path = join(&[
                &crate::protect::path("camera", &d.protect)?,
                "/snapshot?highQuality=true",
            ])?;
            let image = self.config.raw(c, "GET", &path, None, 20000).await?;
            let mime = image
                .headers
                .iter()
                .find(|(k, _)| k.eq_ignore_ascii_case("Content-Type"))
                .map(|(_, v)| v.as_str())
                .unwrap_or("");
            if !mime.starts_with("image/") {
                return Err(Error::Invalid("console returned no image"));
            }
            let id = self
                .serial
                .checked_add(1)
                .ok_or(Error::Invalid("media ids exhausted"))?;
            let token = stulp_protocol::token::base64(&c.random()?)?;
            let url = c.media_url(&token)?;
            let result = json::fields(&[
                ("deviceId", json::string(device)?),
                ("slot", json::string("snapshot")?),
                ("url", json::string(&url)?),
                ("contentType", json::string(mime)?),
            ])?;
            c.media(MediaCommand::Register {
                id,
                token,
                mime: json::copy(mime)?,
                header: image.body,
            })?;
            self.serial = id;
            return Ok(result);
        }
        if d.driver != "camera"
            || json::text(p, "slot") != "live"
            || !matches!(json::text(p, "kind"), "" | "video")
        {
            return Err(Error::Invalid("unknown camera video slot"));
        }
        if !self.cameras.iter().any(|s| s.device == device) {
            if self.cameras.len() >= 4 {
                return Err(Error::Invalid("four cameras are already streaming"));
            }
            let path = join(&[
                &crate::protect::path("camera", &d.protect)?,
                "/rtsps-stream",
            ])?;
            let input =
                json::parse(br#"{"qualities":["high"]}"#).map_err(stulp_core::Error::from)?;
            let streams = self
                .config
                .call(c, "POST", &path, Some(&input), 20000)
                .await?;
            let address = ["high", "medium", "low", "package"]
                .iter()
                .map(|key| json::text(&streams, key))
                .find(|s| !s.is_empty())
                .ok_or(Error::Invalid("console supplied no camera stream"))?;
            let address = address.strip_suffix("?enableSrtp").unwrap_or(address);
            let camera = rtsp::Camera::new(address, c.now())?;
            let token = stulp_protocol::token::base64(&c.random()?)?;
            let url = c.media_url(&token)?;
            let id = self
                .serial
                .checked_add(1)
                .ok_or(Error::Invalid("camera ids exhausted"))?;
            let (host, port, tls) = camera.target();
            let command = StreamCommand::Open {
                id,
                host: json::copy(host)?,
                port,
                tls,
                device_certificate: true,
            };
            self.cameras
                .try_reserve(1)
                .map_err(|_| stulp_core::Error::Memory)?;
            let live = Live {
                device: json::copy(device)?,
                id,
                camera,
                token,
                url,
                mime: String::new(),
            };
            c.stream(command)?;
            self.serial = id;
            self.cameras.push(live);
        }
        let deadline = c.now().saturating_add(20000);
        loop {
            self.camera_io(c)?;
            let live = self
                .cameras
                .iter()
                .find(|s| s.device == device)
                .ok_or(Error::Invalid("camera stream failed to start"))?;
            if !live.mime.is_empty() {
                return Ok(json::fields(&[
                    ("deviceId", json::string(device)?),
                    ("slot", json::string("live")?),
                    ("url", json::string(&live.url)?),
                    ("contentType", json::string(&live.mime)?),
                ])?);
            }
            if c.now() >= deadline {
                self.close_cameras(c, Some(device))?;
                return Err(Error::Timeout);
            }
            c.idle().await?;
        }
    }
}
pub(super) async fn register<T: Transport>(c: &mut Client<T>, id: &str) -> Result {
    let title = json::copy(json::text(c.state().device(id)?, "name"))?;
    let mut media = Vec::new();
    for (slot, kind) in [("snapshot", "image"), ("live", "video")] {
        json::push(
            &mut media,
            json::fields(&[
                ("deviceId", json::string(id)?),
                ("slot", json::string(slot)?),
                ("title", json::string(&title)?),
                ("kind", json::string(kind)?),
                ("resourceId", json::string(slot)?),
            ])?,
            2,
        )?;
    }
    c.call(
        "media.register",
        &json::fields(&[
            ("deviceId", json::string(id)?),
            ("media", Value::Array(media)),
        ])?,
    )
    .await?;
    Ok(())
}
