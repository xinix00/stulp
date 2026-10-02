//! Mediametadata is vluchtig; private bronadressen blijven bij de app.
use super::*;
pub(super) struct SharedImage {
    id: String,
    device: String,
    slot: String,
    expires: u64,
}

impl<S: Storage> Store<S> {
    /// Vervangt de gedeclareerde slots van uitsluitend een eigen apparaat.
    pub fn register_media(&mut self, app: &str, params: &Value) -> Result {
        let id = json::text(params, "deviceId");
        if json::text(self.document.record("devices", id)?, "appId") != app {
            return Err(Error::Invalid("media device belongs to another app"));
        }
        if json::get(params, "media").is_some_and(|v| !v.is_null() && v.as_array().is_none()) {
            return Err(Error::Invalid("media must be an array"));
        }
        let mut slots = Vec::new();
        for item in json::array(params, "media") {
            let slot = json::text(item, "slot");
            let kind = json::text(item, "kind");
            if slot.is_empty()
                || slot.len() > 128
                || slot.contains('/')
                || !matches!(kind, "video" | "image")
            {
                return Err(Error::Invalid("invalid media slot"));
            }
            if slots
                .iter()
                .any(|v| json::text(v, "slot") == slot && json::text(v, "kind") == kind)
            {
                return Err(Error::Conflict("duplicate media slot"));
            }
            let mut public = json::fields(&[
                ("deviceId", json::string(id)?),
                ("slot", json::string(slot)?),
                ("kind", json::string(kind)?),
            ])?;
            for key in ["title", "resourceId"] {
                let text = match json::get(item, key) {
                    Some(v) if !v.is_null() => v
                        .as_str()
                        .ok_or(Error::Invalid("media label must be a string"))?,
                    _ => "",
                };
                if text.len() > 512 {
                    return Err(Error::Full);
                }
                json::set(&mut public, key, json::string(text)?)?;
            }
            if let Some(options) = json::get(item, "options").filter(|v| !v.is_null()) {
                if options.as_object().is_none() {
                    return Err(Error::Invalid("media options must be an object"));
                }
                json::set(&mut public, "options", options.try_clone()?)?;
            }
            json::push(&mut slots, public, 16)?;
        }
        let value = Value::Array(slots);
        if let Some(entry) = self.media.iter_mut().find(|(key, _)| key == id) {
            entry.1 = value;
        } else {
            json::push(&mut self.media, (json::copy(id)?, value), MAX_RECORDS)?;
        }
        Ok(())
    }
    /// Browsermetadata voor een bekend apparaat; nooit een externe stream-URL.
    pub fn device_media(&self, id: &str) -> Result<Value> {
        let d = self.document.record("devices", id)?;
        if self.app_status(json::text(d, "appId")) != "running" {
            return Ok(Value::Array(Vec::new()));
        }
        self.media
            .iter()
            .find(|(key, _)| key == id)
            .map(|(_, v)| v.try_clone().map_err(Error::from))
            .unwrap_or_else(|| Ok(Value::Array(Vec::new())))
    }
    /// Een verbroken attach trekt zijn eerdere mediadeclaraties in.
    pub fn clear_app_media(&mut self, app: &str) {
        self.media.retain(|(id, _)| {
            self.document
                .record("devices", id)
                .is_ok_and(|d| json::text(d, "appId") != app)
        });
    }
    /// Aangemelde stilstaande beelden, bestemd voor de Notify-keuzelijst.
    pub fn image_sources(&self) -> Result<Value> {
        let mut images = Vec::new();
        for d in self.document.records("devices") {
            let media = self.device_media(json::text(d, "id"))?;
            for m in media
                .as_array()
                .unwrap_or(&[])
                .iter()
                .filter(|v| json::text(v, "kind") == "image")
            {
                let name = json::text(d, "name");
                let title = json::text(m, "title");
                json::push(
                    &mut images,
                    json::fields(&[
                        ("deviceId", json::string(json::text(d, "id"))?),
                        ("deviceName", json::string(name)?),
                        ("slot", json::string(json::text(m, "slot"))?),
                        (
                            "title",
                            json::string(if title.is_empty() { name } else { title })?,
                        ),
                    ])?,
                    4096,
                )?;
            }
        }
        Ok(Value::Array(images))
    }
    /// Bewaart uitsluitend een resolver. De caller levert een onraadbaar OS-entropy-id.
    pub fn share_image(&mut self, id: &str, device: &str, slot: &str, now: u64) -> Result<Value> {
        if id.len() < 32
            || !id
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"-_".contains(&b))
            || self.images.iter().any(|v| v.id == id)
        {
            return Err(Error::Invalid("invalid image ticket"));
        }
        let media = self.device_media(device)?;
        let source = media
            .as_array()
            .and_then(|a| {
                a.iter().find(|m| {
                    json::text(m, "kind") == "image"
                        && (slot.is_empty() || json::text(m, "slot") == slot)
                })
            })
            .ok_or(Error::Missing("device has no image slot"))?;
        let ticket = SharedImage {
            id: json::copy(id)?,
            device: json::copy(device)?,
            slot: json::copy(json::text(source, "slot"))?,
            expires: now.saturating_add(900000),
        };
        let mut url = json::copy("/image/")?;
        url.try_reserve(id.len()).map_err(|_| Error::Memory)?;
        url.push_str(id);
        let answer = json::fields(&[("url", json::string(&url)?)])?;
        self.images.try_reserve(1).map_err(|_| Error::Memory)?;
        self.images.retain(|t| t.expires > now);
        if self.images.len() == 8 {
            self.images.remove(0);
        }
        self.images.push(ticket);
        Ok(answer)
    }
    /// Publieke ticketroute; geen browsersessie nodig, wel een geldig tijdelijk handvat.
    pub fn image_source(&self, id: &str, now: u64) -> Result<(&str, Value)> {
        let t = self
            .images
            .iter()
            .find(|t| t.id == id && t.expires > now)
            .ok_or(Error::Missing("image does not exist"))?;
        let d = self.document.record("devices", &t.device)?;
        let slots = self.device_media(&t.device)?;
        if !slots.as_array().is_some_and(|a| {
            a.iter()
                .any(|v| json::text(v, "slot") == t.slot && json::text(v, "kind") == "image")
        }) {
            return Err(Error::Missing("image source unavailable"));
        }
        Ok((
            json::text(d, "appId"),
            json::fields(&[
                ("deviceId", json::string(&t.device)?),
                ("slot", json::string(&t.slot)?),
                ("kind", json::string("image")?),
            ])?,
        ))
    }
}
