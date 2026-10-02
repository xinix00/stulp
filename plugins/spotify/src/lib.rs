//! Spotify Connect bestuurt bestaande spelers; er loopt geen audiostream door deze plugin.
#![no_std]
#![forbid(unsafe_code)]
extern crate alloc;
use alloc::{string::String, vec::Vec};
use stulp_core::json::{self, Value};
use stulp_sdk::{
    Client, Error, Plugin, Result, Transport, clone,
    util::{device_arg, field, float, join, number},
};
mod session;
pub use session::code_from_redirect;
use session::{Session, tokens};
struct Player {
    id: String,
    spotify: String,
}
/// Eén accountpoll bedient alle spelers, met één eigenaar voor tokenrotatie.
#[derive(Default)]
pub struct Spotify {
    session: Session,
    players: Vec<Player>,
    pairs: Vec<String>,
    next: u64,
}
impl Spotify {
    async fn devices<T: Transport>(&mut self, c: &mut Client<T>, pair: bool) -> Result<Value> {
        let answer = self
            .session
            .request(c, "GET", "/me/player/devices", &Value::Null)
            .await?;
        let mut out = Vec::new();
        for d in json::array(&answer, "devices") {
            let item = if pair {
                if json::text(d, "id").is_empty() || json::boolean(d, "is_restricted") {
                    continue;
                }
                json::fields(&[
                    ("name", clone(field(d, "name"))?),
                    ("data", json::fields(&[("id", clone(field(d, "id"))?)])?),
                    (
                        "store",
                        json::fields(&[("type", clone(field(d, "type"))?)])?,
                    ),
                ])?
            } else {
                json::fields(&[
                    ("name", clone(field(d, "name"))?),
                    ("type", clone(field(d, "type"))?),
                    ("active", Value::Bool(json::boolean(d, "is_active"))),
                    ("restricted", Value::Bool(json::boolean(d, "is_restricted"))),
                ])?
            };
            json::push(&mut out, item, 4096)?;
        }
        if pair {
            Ok(Value::Array(out))
        } else {
            Ok(json::fields(&[
                ("linked", Value::Bool(true)),
                ("players", Value::Array(out)),
            ])?)
        }
    }
    async fn api<T: Transport>(&mut self, c: &mut Client<T>, p: &Value) -> Result<Value> {
        match json::text(p, "handler") {
            "status" => {
                let mut answer = json::fields(&[
                    (
                        "clientId",
                        clone(c.state().setting("clientId").unwrap_or(&Value::Null))?,
                    ),
                    (
                        "redirectUri",
                        clone(c.state().setting("redirectUri").unwrap_or(&Value::Null))?,
                    ),
                    ("linked", Value::Bool(!tokens(c).is_null())),
                    ("waiting", Value::Bool(self.session.pending.is_some())),
                    ("devices", Value::uint(self.players.len() as u64)),
                    ("error", json::string(&self.session.error)?),
                ])?;
                if !tokens(c).is_null() {
                    json::set(&mut answer, "expiresAt", clone(field(tokens(c), "expiry"))?)?;
                }
                Ok(answer)
            }
            "authorize" => self.session.authorize(c),
            "exchange" => {
                self.session
                    .exchange(c, json::text(field(p, "body"), "redirect"))
                    .await?;
                self.next = 0;
                self.devices(c, false).await
            }
            "check" => self.devices(c, false).await,
            "disconnect" => {
                c.app_state(Value::Null).await?;
                self.session.pending = None;
                self.session.error.clear();
                Ok(json::fields(&[("linked", Value::Bool(false))])?)
            }
            _ => Err(Error::Invalid("unknown Spotify API handler")),
        }
    }
    async fn capability<T: Transport>(&mut self, c: &mut Client<T>, p: &Value) -> Result<Value> {
        let player = self
            .players
            .iter()
            .find(|v| v.id == json::text(p, "deviceId"))
            .ok_or(Error::Invalid("Kies een speler die bij deze app hoort."))?;
        let mut body = Value::Null;
        let mut volume = String::new();
        let (method, action) = match json::text(p, "capability") {
            "speaker_playing" => match field(p, "value").as_bool() {
                Some(true) => {
                    body = json::object();
                    ("PUT", "play")
                }
                Some(false) => ("PUT", "pause"),
                None => return Err(Error::Invalid("speaker_playing verwacht aan of uit.")),
            },
            "speaker_next" => ("POST", "next"),
            "speaker_prev" => ("POST", "previous"),
            "volume_set" => {
                let n = number(field(p, "value"))
                    .ok_or(Error::Invalid("Volume moet een getal zijn."))?;
                let n = (n.clamp(0.0, 1.0) * 100.0 + 0.5) as u64;
                volume = join(&[
                    "&volume_percent=",
                    &json::to_string(&Value::uint(n)).map_err(stulp_core::Error::from)?,
                ])?;
                ("PUT", "volume")
            }
            _ => return Err(Error::Invalid("unknown Spotify capability")),
        };
        let path = join(&[
            "/me/player/",
            action,
            "?device_id=",
            &stulp_sdk::query(&player.spotify)?,
            &volume,
        ])?;
        self.session.request(c, method, &path, &body).await?;
        self.next = c.now().saturating_add(1000);
        Ok(Value::Null)
    }
    async fn flow<T: Transport>(&mut self, c: &mut Client<T>, p: &Value) -> Result<Value> {
        if json::text(p, "kind") != "action" {
            return Err(Error::Invalid("unknown Spotify card type"));
        }
        let kind = match json::text(p, "id") {
            "play_track" => "track",
            "play_playlist" => "playlist",
            _ => return Err(Error::Invalid("unknown Spotify card")),
        };
        let args = field(p, "args");
        let player = self
            .players
            .iter()
            .find(|v| v.id == device_arg(args))
            .ok_or(Error::Invalid("Kies eerst een Spotify-speler."))?;
        let arg = field(args, kind);
        let chosen = arg.as_str().unwrap_or_else(|| json::text(arg, "id"));
        let name = match json::text(arg, "name") {
            "" => chosen,
            s => s,
        };
        let uri = uri(chosen, kind)?;
        let body = if kind == "track" {
            let mut items = Vec::new();
            json::push(&mut items, json::string(&uri)?, 1)?;
            json::fields(&[("uris", Value::Array(items))])?
        } else {
            json::fields(&[("context_uri", json::string(&uri)?)])?
        };
        let path = join(&[
            "/me/player/play?device_id=",
            &stulp_sdk::query(&player.spotify)?,
        ])?;
        self.session.request(c, "PUT", &path, &body).await?;
        self.next = c.now().saturating_add(1000);
        Ok(json::fields(&[(kind, json::string(name)?)])?)
    }
    async fn autocomplete<T: Transport>(&mut self, c: &mut Client<T>, p: &Value) -> Result<Value> {
        let (kind, list) = match (
            json::text(p, "kind"),
            json::text(p, "id"),
            json::text(p, "argument"),
        ) {
            ("action", "play_track", "track") => ("track", "tracks"),
            ("action", "play_playlist", "playlist") => ("playlist", "playlists"),
            _ => return Err(Error::Invalid("unknown Spotify autocomplete")),
        };
        let query = json::text(p, "query");
        let mut out = Vec::new();
        if query.trim().is_empty() {
            return Ok(Value::Array(out));
        }
        // Tien is de gemeten grens uit de Go-versie (2026-08-10), ook voor playlists.
        let path = join(&[
            "/search?type=",
            kind,
            "&limit=10&q=",
            &stulp_sdk::query(query)?,
        ])?;
        let answer = self.session.request(c, "GET", &path, &Value::Null).await?;
        for item in json::array(field(&answer, list), "items") {
            if item.is_null() || json::text(item, "uri").is_empty() {
                continue;
            }
            let by = if kind == "track" {
                artists(item)?
            } else {
                let owner = json::text(field(item, "owner"), "display_name");
                json::copy(if owner.trim().is_empty() {
                    json::text(item, "description")
                } else {
                    owner
                })?
            };
            let images = if kind == "track" {
                field(item, "album")
            } else {
                item
            };
            json::push(
                &mut out,
                json::fields(&[
                    ("id", clone(field(item, "uri"))?),
                    ("name", clone(field(item, "name"))?),
                    ("description", json::string(&by)?),
                    ("image", json::string(cover(images))?),
                ])?,
                50,
            )?;
        }
        Ok(Value::Array(out))
    }
    async fn sweep<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        let devices = match self
            .session
            .request(c, "GET", "/me/player/devices", &Value::Null)
            .await
        {
            Ok(v) => v,
            Err(e) => {
                let reason = stulp_sdk::message(&e)?;
                for p in &self.players {
                    c.unavailable(&p.id, &reason).await?;
                }
                return Ok(());
            }
        };
        let playback = self
            .session
            .request(c, "GET", "/me/player", &Value::Null)
            .await
            .unwrap_or(Value::Null);
        for p in &mut self.players {
            let own = c.state().device(&p.id)?;
            let name = json::text(own, "name");
            let found = json::array(&devices, "devices")
                .iter()
                .find(|d| json::text(d, "id") == p.spotify)
                .or_else(|| {
                    json::array(&devices, "devices").iter().find(|d| {
                        !json::text(d, "name").is_empty() && json::text(d, "name") == name
                    })
                });
            let Some(d) = found else {
                c.unavailable(
                    &p.id,
                    "Spotify ziet deze speler nu niet. Zet hem aan of open de app erop.",
                )
                .await?;
                c.values(
                    &p.id,
                    json::fields(&[("speaker_playing", Value::Bool(false))])?,
                )
                .await?;
                continue;
            };
            let id = json::text(d, "id");
            if id != p.spotify {
                let new = json::copy(id)?;
                c.store(&p.id, json::fields(&[("spotifyId", json::string(id)?)])?)
                    .await?;
                p.spotify = new;
            }
            let playing = !playback.is_null() && json::text(field(&playback, "device"), "id") == id;
            let item = if playing {
                field(&playback, "item")
            } else {
                &Value::Null
            };
            let mut values = json::fields(&[
                (
                    "speaker_playing",
                    Value::Bool(playing && json::boolean(&playback, "is_playing")),
                ),
                ("speaker_track", json::string(json::text(item, "name"))?),
                ("speaker_artist", json::string(&artists(item)?)?),
                (
                    "speaker_album",
                    json::string(json::text(field(item, "album"), "name"))?,
                ),
            ])?;
            if let Some(volume) = number(field(d, "volume_percent")) {
                json::set(&mut values, "volume_set", float(volume / 100.0)?)?;
            }
            c.values(&p.id, values).await?;
            c.available(&p.id, true).await?;
        }
        Ok(())
    }
}
impl Plugin for Spotify {
    fn assets(&self) -> &'static [&'static str] {
        &["settings/index.html", "settings/page.js"]
    }
    fn manifest(&self) -> &'static [u8] {
        include_bytes!("../app.json")
    }
    async fn handle<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        method: &str,
        p: &Value,
    ) -> Result<Value> {
        match method {
            "app.init" => {
                self.session.next = c.now().saturating_add(60_000);
                self.next = c.now().saturating_add(1000);
                Ok(Value::Null)
            }
            "driver.init" => {
                if json::text(p, "driverId") == "player" {
                    Ok(Value::Null)
                } else {
                    Err(Error::Invalid("unknown Spotify driver"))
                }
            }
            "device.init" => {
                let id = json::text(p, "deviceId");
                let d = c.state().device(id)?;
                let data = json::text(field(d, "data"), "id");
                if data.is_empty() || json::text(d, "driverId") != "player" {
                    return Err(Error::Invalid(
                        "Dit apparaat heeft geen Spotify-id; koppel opnieuw.",
                    ));
                }
                let spotify = match json::text(field(d, "store"), "spotifyId") {
                    "" => data,
                    s => s,
                };
                let player = Player {
                    id: json::copy(id)?,
                    spotify: json::copy(spotify)?,
                };
                self.players.retain(|p| p.id != id);
                json::push(&mut self.players, player, 4096)?;
                self.next = c.now().saturating_add(1000);
                Ok(Value::Null)
            }
            "device.delete" => {
                self.players.retain(|v| v.id != json::text(p, "deviceId"));
                Ok(Value::Null)
            }
            "capability.invoke" => self.capability(c, p).await,
            "capabilities.invoke" => {
                let mut failed = json::object();
                for cmd in json::array(p, "commands") {
                    let mut command = clone(cmd)?;
                    json::set(&mut command, "deviceId", clone(field(p, "deviceId"))?)?;
                    if let Err(e) = self.capability(c, &command).await {
                        json::set(
                            &mut failed,
                            json::text(cmd, "capability"),
                            json::string(&stulp_sdk::message(&e)?)?,
                        )?;
                    }
                }
                Ok(failed)
            }
            "api.invoke" => self.api(c, p).await,
            "flow.run" => self.flow(c, p).await,
            "flow.autocomplete" => self.autocomplete(c, p).await,
            "pair.list" => {
                if json::text(p, "driverId") != "player" {
                    return Err(Error::Invalid("unknown Spotify driver"));
                }
                self.devices(c, true).await
            }
            "pair.start" => {
                let id = json::text(p, "sessionId");
                if id.is_empty()
                    || json::text(p, "driverId") != "player"
                    || self.pairs.iter().any(|s| s == id)
                {
                    return Err(Error::Invalid("invalid pairing session"));
                }
                json::push(&mut self.pairs, json::copy(id)?, 32)?;
                json::parse(br#"["list_devices"]"#).map_err(|e| Error::Core(e.into()))
            }
            "pair.emit" => {
                if json::text(p, "event") != "list_devices"
                    || !self.pairs.iter().any(|s| s == json::text(p, "sessionId"))
                {
                    return Err(Error::Invalid("invalid pairing event"));
                }
                self.devices(c, true).await
            }
            "pair.close" => {
                self.pairs.retain(|s| s != json::text(p, "sessionId"));
                Ok(Value::Null)
            }
            "registrations" => {
                let mut r = stulp_sdk::registrations(
                    &json::parse(self.manifest()).map_err(stulp_core::Error::from)?,
                )?;
                let mut cards = Vec::new();
                for card in json::array(&r, "flows") {
                    let mut card = clone(card)?;
                    let arg = if json::text(&card, "id") == "play_track" {
                        "track"
                    } else {
                        "playlist"
                    };
                    let mut args = Vec::new();
                    json::push(&mut args, json::string(arg)?, 1)?;
                    json::set(&mut card, "autocomplete", Value::Array(args))?;
                    json::push(&mut cards, card, 2)?;
                }
                json::set(&mut r, "flows", Value::Array(cards))?;
                Ok(r)
            }
            "ui.asset" => match json::text(p, "path") {
                "settings/index.html" => stulp_sdk::asset(include_bytes!("../settings/index.html")),
                "settings/page.js" => stulp_sdk::asset(include_bytes!("../settings/page.js")),
                _ => Ok(json::fields(&[("found", Value::Bool(false))])?),
            },
            _ => Err(Error::Invalid("unknown Spotify method")),
        }
    }
    async fn tick<T: Transport>(&mut self, c: &mut Client<T>) -> Result {
        if self.session.next <= c.now() {
            self.session.next = c.now().saturating_add(60_000);
            if !tokens(c).is_null()
                && let Err(e) = self.session.access(c).await
            {
                self.session.error = stulp_sdk::message(&e)?;
            }
            return Ok(());
        }
        if self.next <= c.now() {
            self.next = c.now().saturating_add(10_000);
            if !self.players.is_empty() {
                self.sweep(c).await?;
            }
        }
        Ok(())
    }
}
/// URI's en bestaande gedeelde playlistlinks, nooit een losse zoekterm.
pub fn uri(value: &str, kind: &str) -> Result<String> {
    if !matches!(kind, "track" | "playlist") {
        return Err(Error::Invalid("unknown Spotify URI kind"));
    }
    let value = value.trim();
    let prefix = join(&["spotify:", kind, ":"])?;
    if value.starts_with(&prefix) {
        return Ok(json::copy(value)?);
    }
    let value = if kind == "playlist" {
        value
            .strip_prefix("https://open.spotify.com/playlist/")
            .map(|s| s.split('?').next().unwrap_or(""))
            .unwrap_or(value)
    } else {
        value
    };
    if value.len() == 22 && value.bytes().all(|b| b.is_ascii_alphanumeric()) {
        return join(&[&prefix, value]);
    }
    Err(Error::Invalid(
        "Kies een nummer of playlist uit de lijst; losse zoektekst kan niet worden afgespeeld.",
    ))
}
fn artists(track: &Value) -> Result<String> {
    let mut out = String::new();
    for (i, a) in json::array(track, "artists").iter().enumerate() {
        let name = json::text(a, "name");
        out.try_reserve(name.len() + 2)
            .map_err(|_| stulp_core::Error::Memory)?;
        if i > 0 {
            out.push_str(", ");
        }
        out.push_str(name);
    }
    Ok(out)
}
fn cover(value: &Value) -> &str {
    let mut best = "";
    let mut size = 0;
    for image in json::array(value, "images") {
        let width = json::uint(image, "width");
        if best.is_empty() || (width > 0 && width < size) {
            best = json::text(image, "url");
            size = width;
        }
    }
    best
}
