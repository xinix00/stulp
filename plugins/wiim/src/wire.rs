//! De oorspronkelijke GetInfoEx-, DIDL- en httpapi-contracten van WiiM.
use alloc::{string::String, vec::Vec};
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Client, Error, HttpRequest, Result, Transport,
    util::{float, join},
    xml::Document,
};
pub(super) const AV: &str = "urn:schemas-upnp-org:service:AVTransport:1";
pub(super) fn address(s: &str) -> Result {
    if s.is_empty()
        || s.bytes()
            .any(|b| b <= b' ' || b == 127 || b"/:?&#@\\[]".contains(&b))
    {
        return Err(Error::Invalid(
            "Vul alleen het IP-adres of de hostnaam van de speler in.",
        ));
    }
    Ok(())
}
pub(super) fn description_url(host: &str, port: u16) -> Result<String> {
    address(host)?;
    join(&[
        "http://",
        host,
        ":",
        &crate::decimal(u64::from(port))?,
        "/description.xml",
    ])
}
pub(super) fn host_port(url: &str) -> Result<(&str, u16)> {
    let tail = url
        .strip_prefix("http://")
        .or_else(|| url.strip_prefix("https://"))
        .ok_or(Error::Invalid("UPnP URL is geen HTTP-adres"))?;
    let authority = tail.split(['/', '?', '#']).next().unwrap_or("");
    let (host, port) = if let Some((host, p)) = authority.split_once(':') {
        (
            host,
            p.parse::<u16>()
                .ok()
                .filter(|p| *p != 0)
                .ok_or(Error::Invalid("ongeldige UPnP-poort"))?,
        )
    } else {
        (authority, 49152)
    };
    address(host)?;
    Ok((host, port))
}
fn resolve(base: &str, reference: &str) -> Result<String> {
    if reference.starts_with("http://") || reference.starts_with("https://") {
        host_port(reference)?;
        return Ok(json::copy(reference)?);
    }
    let (scheme, tail) = base
        .split_once("://")
        .ok_or(Error::Invalid("invalid UPnP URLBase"))?;
    if !matches!(scheme, "http" | "https") {
        return Err(Error::Invalid("invalid UPnP URLBase"));
    }
    let origin_end = tail.find('/').unwrap_or(tail.len());
    let authority = &tail[..origin_end];
    if reference.starts_with("//") {
        return join(&[scheme, ":", reference]);
    }
    let path = if reference.starts_with('/') {
        json::copy(reference)?
    } else {
        let path = tail
            .get(origin_end..)
            .unwrap_or("/")
            .split(['?', '#'])
            .next()
            .unwrap_or("/");
        let directory = path.rfind('/').map_or("/", |i| &path[..i + 1]);
        join(&[directory, reference])?
    };
    let mut parts = Vec::new();
    for part in path.split('/') {
        match part {
            "." => (),
            ".." => {
                parts.pop();
            }
            _ => json::push(&mut parts, part, 1024)?,
        }
    }
    let mut normalized = String::new();
    normalized
        .try_reserve(path.len())
        .map_err(|_| stulp_core::Error::Memory)?;
    for (i, part) in parts.iter().enumerate() {
        if i != 0 {
            normalized.push('/');
        }
        normalized.push_str(part);
    }
    let url = join(&[scheme, "://", authority, &normalized])?;
    host_port(&url)?;
    Ok(url)
}
pub(super) struct Description {
    pub(super) uuid: String,
    pub(super) name: String,
    pub(super) model: String,
    pub(super) address: String,
    pub(super) port: u16,
    pub(super) control: String,
    pub(super) player: bool,
}
impl Description {
    pub(super) fn parse(location: &str, bytes: &[u8]) -> Result<Self> {
        let doc = Document::parse(bytes)?;
        let root = doc
            .find("root")
            .ok_or(Error::Invalid("UPnP root ontbreekt"))?;
        let device = doc
            .child(root, "device")
            .ok_or(Error::Invalid("UPnP device ontbreekt"))?;
        let udn = doc.field(device, "UDN");
        let uuid = udn.strip_prefix("uuid:").unwrap_or(udn);
        if uuid.is_empty() {
            return Err(Error::Invalid("UPnP UDN ontbreekt"));
        }
        let (host, port) = host_port(location)?;
        let name = doc.field(device, "friendlyName");
        let model = doc.field(device, "modelName");
        let name = if !name.is_empty() {
            name
        } else if !model.is_empty() {
            model
        } else {
            host
        };
        let mut brand = contains_ignore_case(doc.field(device, "manufacturer"), "linkplay")
            || contains_ignore_case(model, "wiim");
        let mut control = String::new();
        if let Some(list) = doc.child(device, "serviceList") {
            for s in doc.children(list, "service") {
                let kind = doc.field(s, "serviceType");
                brand |= contains_ignore_case(kind, "wiimu-com");
                if kind.eq_ignore_ascii_case(AV) && !doc.field(s, "controlURL").is_empty() {
                    let base = doc.field(root, "URLBase");
                    control = resolve(
                        if base.is_empty() { location } else { base },
                        doc.field(s, "controlURL"),
                    )?;
                }
            }
        }
        Ok(Self {
            uuid: json::copy(uuid)?,
            name: json::copy(name)?,
            model: json::copy(model)?,
            address: json::copy(host)?,
            port,
            player: brand && !control.is_empty(),
            control,
        })
    }
    pub(super) fn paired(&self) -> Result<Value> {
        Ok(json::fields(&[
            ("name", json::string(&self.name)?),
            ("data", json::fields(&[("id", json::string(&self.uuid)?)])?),
            (
                "settings",
                json::fields(&[("address", json::string(&self.address)?)])?,
            ),
            (
                "store",
                json::fields(&[
                    ("model", json::string(&self.model)?),
                    ("port", Value::uint(u64::from(self.port))),
                ])?,
            ),
        ])?)
    }
    pub(super) fn summary(&self) -> Result<Value> {
        Ok(json::fields(&[
            ("uuid", json::string(&self.uuid)?),
            ("name", json::string(&self.name)?),
            ("model", json::string(&self.model)?),
            ("address", json::string(&self.address)?),
        ])?)
    }
}
pub(super) async fn describe<T: Transport>(
    c: &mut Client<T>,
    url: &str,
    timeout: u64,
) -> Result<Description> {
    let mut req = HttpRequest::get(url)?;
    req.timeout_ms = timeout;
    req.device_certificate = url.starts_with("https://");
    let reply = c.http(req).await?;
    if !(200..300).contains(&reply.status) {
        return Err(Error::Invalid("UPnP-beschrijving werd geweigerd"));
    }
    Description::parse(url, &reply.body)
}
pub(super) async fn status<T: Transport>(
    c: &mut Client<T>,
    control: &str,
    timeout: u64,
) -> Result<Value> {
    let mut req = HttpRequest::get(control)?;
    req.timeout_ms = timeout;
    req.method = json::copy("POST")?;
    req.device_certificate = control.starts_with("https://");
    req.body=join(&["<?xml version=\"1.0\" encoding=\"utf-8\"?><s:Envelope xmlns:s=\"http://schemas.xmlsoap.org/soap/envelope/\" s:encodingStyle=\"http://schemas.xmlsoap.org/soap/encoding/\"><s:Body><u:GetInfoEx xmlns:u=\"",AV,"\"><InstanceID>0</InstanceID></u:GetInfoEx></s:Body></s:Envelope>"])?.into_bytes();
    json::push(
        &mut req.headers,
        (
            json::copy("Content-Type")?,
            json::copy("text/xml; charset=\"utf-8\"")?,
        ),
        2,
    )?;
    json::push(
        &mut req.headers,
        (
            json::copy("SOAPACTION")?,
            join(&["\"", AV, "#GetInfoEx\""])?,
        ),
        2,
    )?;
    let response = c.http(req).await?;
    let value = parse_status(&response.body)?;
    if response.status >= 400 {
        return Err(Error::Invalid("GetInfoEx werd geweigerd"));
    }
    Ok(value)
}
pub(super) fn parse_status(bytes: &[u8]) -> Result<Value> {
    let doc = Document::parse(bytes)?;
    if doc.find("Fault").is_some() {
        let text = doc
            .find("errorDescription")
            .or_else(|| doc.find("faultstring"))
            .map_or("Onbekende UPnP-fout", |i| doc.text(i));
        let code = doc.find("errorCode").map_or("", |i| doc.text(i));
        return Err(Error::Remote(join(&[
            "GetInfoEx: ",
            text,
            " (",
            code,
            ")",
        ])?));
    }
    let response = doc
        .find("GetInfoExResponse")
        .ok_or(Error::Invalid("GetInfoExResponse ontbreekt"))?;
    for required in [
        "CurrentTransportState",
        "LoopMode",
        "TrackDuration",
        "RelTime",
        "CurrentVolume",
        "CurrentMute",
        "TrackMetaData",
    ] {
        if doc.child(response, required).is_none() {
            return Err(Error::Remote(join(&[
                "Het antwoord van de speler mist ",
                required,
            ])?));
        }
    }
    let state = doc.field(response, "CurrentTransportState");
    let volume = doc
        .field(response, "CurrentVolume")
        .parse::<f64>()
        .ok()
        .filter(|v| v.is_finite())
        .ok_or(Error::Invalid("CurrentVolume is geen getal"))?;
    let mut values = json::fields(&[
        ("speaker_playing", Value::Bool(state == "PLAYING")),
        ("volume_set", float(volume.clamp(0., 100.) / 100.)?),
        (
            "volume_mute",
            Value::Bool(doc.field(response, "CurrentMute") == "1"),
        ),
    ])?;
    if let Some((shuffle, repeat)) = loop_mode(doc.field(response, "LoopMode")) {
        json::set(&mut values, "speaker_shuffle", Value::Bool(shuffle))?;
        json::set(&mut values, "speaker_repeat", json::string(repeat)?)?;
    }
    for (src, dst) in [
        ("TrackDuration", "speaker_duration"),
        ("RelTime", "speaker_position"),
    ] {
        if let Some(seconds) = seconds(doc.field(response, src))? {
            json::set(&mut values, dst, float(seconds)?)?;
        }
    }
    let metadata = doc.field(response, "TrackMetaData");
    let (artist, album, title) = if state != "NO_MEDIA_PRESENT" && !metadata.is_empty() {
        track(metadata)?
    } else {
        (String::new(), String::new(), String::new())
    };
    for (k, v) in [
        ("speaker_artist", artist),
        ("speaker_album", album),
        ("speaker_track", title),
    ] {
        json::set(&mut values, k, Value::String(v))?;
    }
    Ok(values)
}
fn track(text: &str) -> Result<(String, String, String)> {
    let doc = Document::parse(text.as_bytes())?;
    let Some(root) = doc.find("DIDL-Lite") else {
        return Err(Error::Invalid("DIDL-Lite ontbreekt"));
    };
    let Some(item) = doc.child(root, "item") else {
        return Ok((String::new(), String::new(), String::new()));
    };
    let title = doc.field(item, "title");
    let subtitle = doc.field(item, "subtitle");
    let artist = doc.field(item, "artist");
    let album = doc.field(item, "album");
    if !subtitle.is_empty() {
        return Ok((
            json::copy(title)?,
            json::copy(title)?,
            json::copy(subtitle)?,
        ));
    }
    let artist = if artist.is_empty() {
        json::copy(album)?
    } else if album.is_empty() {
        json::copy(artist)?
    } else {
        join(&[artist, ", ", album])?
    };
    Ok((artist, json::copy(album)?, json::copy(title)?))
}
pub(super) fn seconds(s: &str) -> Result<Option<f64>> {
    if s.is_empty() || s.eq_ignore_ascii_case("NOT_IMPLEMENTED") {
        return Ok(None);
    }
    let mut parts = s.split(':');
    let mut total = 0.;
    for _ in 0..3 {
        let n = parts
            .next()
            .and_then(|v| v.parse::<f64>().ok())
            .filter(|v| v.is_finite() && *v >= 0.)
            .ok_or(Error::Invalid("tijd is geen uu:mm:ss"))?;
        total = total * 60. + n;
    }
    if parts.next().is_some() || !total.is_finite() {
        return Err(Error::Invalid("ongeldige tijd"));
    }
    Ok(Some(total))
}
const LOOPS: [(bool, &str); 6] = [
    (false, "playlist"),
    (false, "track"),
    (true, "playlist"),
    (true, "none"),
    (false, "none"),
    (true, "track"),
];
pub(super) fn loop_mode(raw: &str) -> Option<(bool, &'static str)> {
    raw.parse::<usize>()
        .ok()
        .and_then(|i| LOOPS.get(i).copied())
}
pub(super) fn encode_loop(shuffle: bool, repeat: &str) -> Result<String> {
    LOOPS
        .iter()
        .position(|v| *v == (shuffle, repeat))
        .ok_or(Error::Invalid("onbekende herhaalstand"))
        .and_then(|i| crate::decimal(i as u64))
}
pub(super) async fn command<T: Transport>(c: &mut Client<T>, host: &str, cmd: &str) -> Result {
    address(host)?;
    if cmd.is_empty()
        || cmd.len() > 200
        || !cmd
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b":_-.".contains(&b))
    {
        return Err(Error::Invalid("ongeldige speleropdracht"));
    }
    let mut req = HttpRequest::get(&join(&["https://", host, "/httpapi.asp?command=", cmd])?)?;
    req.device_certificate = true;
    req.timeout_ms = 10_000;
    let r = c.http(req).await?;
    if !(200..300).contains(&r.status) {
        return Err(Error::Remote(join(&[
            "Speler weigerde ",
            cmd,
            " met HTTP ",
            &crate::decimal(u64::from(r.status))?,
        ])?));
    }
    Ok(())
}
pub(super) fn location(bytes: &[u8]) -> Option<&str> {
    let text = core::str::from_utf8(bytes).ok()?;
    let mut lines = text.lines();
    if !lines.next()?.trim().eq_ignore_ascii_case("HTTP/1.1 200 OK") {
        return None;
    }
    lines
        .filter_map(|line| line.split_once(':'))
        .find(|(k, _)| k.eq_ignore_ascii_case("location"))
        .map(|(_, v)| v.trim())
        .filter(|s| !s.is_empty())
}

fn contains_ignore_case(s: &str, part: &str) -> bool {
    s.as_bytes()
        .windows(part.len())
        .any(|v| v.eq_ignore_ascii_case(part.as_bytes()))
}
