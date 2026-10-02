//! Browserproxies houden manifestrechten, padcontrole en appcallbacks bij dezelfde eigenaar.
use alloc::{string::String, vec::Vec};
use stulp_core::{
    Error, Result,
    json::{self, Value},
    store::{Storage, Store},
};
use stulp_sdk::util::field;
use stulp_web::{Body, Request, Response};
/// Pending plugin UI resource and its locale/context assembly.
pub struct Asset {
    path: String,
    root: String,
    context: Value,
    /// Plugin that owns this resource.
    pub app: String,
    locales: Vec<String>,
    original: Option<Value>,
    locale: Value,
}
fn sdk(e: stulp_sdk::Error) -> Error {
    match e {
        stulp_sdk::Error::Core(e) => e,
        _ => Error::Invalid("invalid app route"),
    }
}
fn join(parts: &[&str]) -> Result<String> {
    stulp_sdk::util::join(parts).map_err(sdk)
}
/// Decode query parameters, preserving repeated values.
pub fn query(raw: &str) -> Result<Value> {
    let mut out = json::object();
    for part in raw.split('&').filter(|s| !s.is_empty()) {
        let (k, v) = part.split_once('=').unwrap_or((part, ""));
        let k = stulp_sdk::util::unquery(k).map_err(sdk)?;
        let v = json::string(&stulp_sdk::util::unquery(v).map_err(sdk)?)?;
        let value = if let Some(old) = json::get(&out, &k) {
            let mut values = Vec::new();
            if let Some(array) = old.as_array() {
                for item in array {
                    json::push(&mut values, stulp_sdk::clone(item).map_err(sdk)?, 128)?;
                }
            } else {
                json::push(&mut values, stulp_sdk::clone(old).map_err(sdk)?, 128)?;
            }
            json::push(&mut values, v, 128)?;
            Value::Array(values)
        } else {
            v
        };
        json::set(&mut out, &k, value)?;
    }
    Ok(out)
}
/// Resolve only API routes declared by the installed manifest.
pub fn api<S: Storage>(
    store: &Store<S>,
    r: &Request,
) -> Result<Option<(String, &'static str, Value)>> {
    let Some(rest) = r.path.strip_prefix("/api/stulp/apps/") else {
        return Ok(None);
    };
    let Some((app, path)) = rest.split_once("/api/") else {
        return Ok(None);
    };
    store.document().record("apps", app)?;
    let manifest = store
        .manifest(app)
        .ok_or(Error::Missing("app manifest unavailable"))?;
    let wanted = join(&["/", path])?;
    let routes = field(manifest, "api")
        .as_object()
        .ok_or(Error::Missing("app API route does not exist"))?;
    let handler = routes
        .iter()
        .find(|(_, route)| {
            json::text(route, "path") == wanted
                && json::text(route, "method").eq_ignore_ascii_case(&r.method)
        })
        .map(|(key, _)| key)
        .ok_or(Error::Missing("app API route does not exist"))?;
    let body = if r.method == "GET" || r.body.is_empty() {
        json::object()
    } else {
        let body = json::parse(&r.body)?;
        if body.as_object().is_none() {
            return Err(Error::Invalid("app API body must be an object"));
        }
        body
    };
    Ok(Some((
        json::copy(app)?,
        "api.invoke",
        json::fields(&[
            ("handler", json::string(handler)?),
            ("query", query(&r.query)?),
            ("body", body),
        ])?,
    )))
}
/// Validate and prepare a plugin page or asset request.
pub fn prepare<S: Storage>(
    store: &Store<S>,
    r: &Request,
) -> Result<Option<(String, Value, Asset)>> {
    if r.method != "GET" {
        return Ok(None);
    }
    let Some(rest) = r.path.strip_prefix("/app-ui/") else {
        return Ok(None);
    };
    let Some((app, rest)) = rest.split_once('/') else {
        return Ok(None);
    };
    let record = store.document().record("apps", app)?;
    let root = json::text(record, "root");
    let mut context = json::fields(&[("appId", json::string(app)?)])?;
    let path = if let Some(relative) = rest.strip_prefix("settings/") {
        let relative = if relative.is_empty() {
            "index.html"
        } else {
            relative
        };
        if !stulp_sdk::valid_asset(relative) {
            return Err(Error::Missing("app asset not found"));
        }
        json::set(&mut context, "origin", json::string("settings")?)?;
        join(&["settings/", relative])?
    } else if let Some(rest) = rest.strip_prefix("pair/") {
        let (driver, relative) = rest
            .split_once('/')
            .ok_or(Error::Missing("app asset not found"))?;
        let relative = if relative.is_empty() {
            "validate.html"
        } else {
            relative
        };
        if !stulp_sdk::valid_asset(driver) || !stulp_sdk::valid_asset(relative) {
            return Err(Error::Missing("app asset not found"));
        }
        json::set(&mut context, "origin", json::string("pair")?)?;
        json::set(&mut context, "driverId", json::string(driver)?)?;
        json::set(
            &mut context,
            "sessionId",
            json::string(json::text(&query(&r.query)?, "session"))?,
        )?;
        join(&["drivers/", driver, "/pair/", relative])?
    } else {
        return Ok(None);
    };
    let manifest = store
        .manifest(app)
        .ok_or(Error::Missing("app manifest unavailable"))?;
    if root.is_empty()
        && !json::array(field(manifest, "ui"), "assets")
            .iter()
            .any(|p| p.as_str() == Some(&path))
    {
        return Err(Error::Missing("app asset not declared"));
    }
    let mut locales = Vec::new();
    if path.ends_with(".html") {
        for language in [store.language(), "en"] {
            let locale = join(&["locales/", language, ".json"])?;
            if stulp_sdk::valid_asset(&locale)
                && !locales.contains(&locale)
                && (!root.is_empty()
                    || json::array(field(manifest, "ui"), "assets")
                        .iter()
                        .any(|v| v.as_str() == Some(&locale)))
            {
                json::push(&mut locales, locale, 2)?;
            }
        }
    }
    Ok(Some((
        json::copy(app)?,
        json::fields(&[("path", json::string(&path)?)])?,
        Asset {
            path,
            root: json::copy(root)?,
            context,
            app: json::copy(app)?,
            locales,
            original: None,
            locale: json::object(),
        },
    )))
}
impl Asset {
    /// Whether this asset belongs to an installed filesystem bundle.
    pub fn is_local(&self) -> bool {
        !self.root.is_empty()
    }
    /// Assemble an installed page through the platform file reader.
    pub fn local_response(
        mut self,
        mut read: impl FnMut(&str, &str) -> Result<Value>,
    ) -> Result<Response> {
        let mut value = read(&self.root, &self.path)?;
        while let Some(params) = self.advance(&value, false)? {
            value = match read(&self.root, json::text(&params, "path")) {
                Ok(value) => value,
                Err(Error::Memory) => return Err(Error::Memory),
                // Locale files are optional, like failed ui.asset callbacks.
                // An unreadable Dutch locale must still allow the English page.
                Err(_) => json::fields(&[("found", Value::Bool(false))])?,
            };
        }
        self.response(&value, false)
    }

    /// Optionele taalbestanden gebruiken dezelfde manifestrechten en callbackbaan als HTML.
    pub fn advance(&mut self, value: &Value, failed: bool) -> Result<Option<Value>> {
        if self.original.is_none() {
            if failed || !json::boolean(value, "found") || self.locales.is_empty() {
                return Ok(None);
            }
            self.original = Some(stulp_sdk::clone(value).map_err(sdk)?);
        } else if !failed
            && json::boolean(value, "found")
            && let Ok(bytes) = stulp_protocol::token::decode(json::text(value, "data"))
            && let Ok(locale) = json::parse(&bytes)
            && locale.as_object().is_some()
        {
            self.locale = locale;
            self.locales.clear();
        }
        if self.locales.is_empty() {
            return Ok(None);
        }
        let path = self.locales.remove(0);
        Ok(Some(json::fields(&[("path", json::string(&path)?)])?))
    }
    /// Assemble the completed plugin callback into a browser response.
    pub fn response(self, value: &Value, failed: bool) -> Result<Response> {
        if failed && self.original.is_none() {
            return Response::error(502, json::text(value, "message"));
        }
        let value = self.original.as_ref().unwrap_or(value);
        if !json::boolean(value, "found") {
            return Response::error(404, "app asset not found");
        }
        let mut bytes = stulp_protocol::token::decode(json::text(value, "data"))?;
        let content_type = if self.path.ends_with(".html") {
            let html =
                String::from_utf8(bytes).map_err(|_| Error::Invalid("app HTML is not UTF-8"))?;
            let context = join(&[
                &json::to_string(&self.context)?,
                ";window.__STULP_LOCALE__=",
                &json::to_string(&self.locale)?,
            ])?;
            let mut safe = String::new();
            safe.try_reserve(context.len().saturating_mul(6))
                .map_err(|_| Error::Memory)?;
            for c in context.chars() {
                match c {
                    '<' => safe.push_str("\\u003c"),
                    '>' => safe.push_str("\\u003e"),
                    '&' => safe.push_str("\\u0026"),
                    '\u{2028}' => safe.push_str("\\u2028"),
                    '\u{2029}' => safe.push_str("\\u2029"),
                    c => safe.push(c),
                }
            }
            let mut injection = join(&[
                "<meta name=\"color-scheme\" content=\"dark\"><link rel=\"stylesheet\" href=\"/assets/app-frame.css\"><script>window.__STULP_CONTEXT__=",
                &safe,
                ";</script>",
            ])?;
            if !has_bridge(&html) {
                injection = join(&[
                    &injection,
                    "<script src=\"/stulp.js\" data-origin=\"",
                    json::text(&self.context, "origin"),
                    "\"></script>",
                ])?;
            }
            let result = if let Some((before, after)) = html.split_once("<head>") {
                join(&[before, "<head>", &injection, after])?
            } else {
                join(&[&injection, &html])?
            };
            bytes = result.into_bytes();
            "text/html; charset=utf-8"
        } else if self.path.ends_with(".js") {
            "text/javascript; charset=utf-8"
        } else if self.path.ends_with(".css") {
            "text/css; charset=utf-8"
        } else if self.path.ends_with(".svg") {
            "image/svg+xml"
        } else if self.path.ends_with(".png") {
            "image/png"
        } else if self.path.ends_with(".json") {
            "application/json"
        } else {
            "application/octet-stream"
        };
        Ok(Response {
            status: 200,
            content_type,
            body: Body::Bytes(bytes),
            cookie: None,
            headers: Vec::new(),
        })
    }
}
fn has_bridge(html: &str) -> bool {
    html.split("<script").skip(1).any(|part| {
        part.split_once('>')
            .is_some_and(|(tag, _)| tag.contains("src=") && tag.contains("stulp.js"))
    })
}

#[cfg(test)]
#[allow(clippy::unwrap_used)]
mod tests {
    use super::*;
    use stulp_core::store::Memory;
    fn asset(text: &str) -> Value {
        json::fields(&[
            ("found", Value::Bool(true)),
            (
                "data",
                json::string(&stulp_protocol::token::base64(text.as_bytes()).unwrap()).unwrap(),
            ),
        ])
        .unwrap()
    }
    #[test]
    fn html_locale_falls_back_and_escapes_script_content() {
        let mut store = Store::open(
            br#"{"version":2,"apps":[{"id":"a","enabled":true}]}"#,
            Memory,
        )
        .unwrap();
        store.announce("a", json::parse(br#"{"id":"a","sdk":3,"version":"1","ui":{"assets":["settings/index.html","locales/nl.json","locales/en.json"]}}"#).unwrap()).unwrap();
        let request = Request {
            method: "GET".into(),
            path: "/app-ui/a/settings/".into(),
            query: String::new(),
            body: Vec::new(),
            cookie: String::new(),
            host: String::new(),
            origin: String::new(),
            headers: json::object(),
        };
        let (_, _, mut page) = prepare(&store, &request).unwrap().unwrap();
        let html = asset("<html><head></head><body>Settings</body></html>");
        assert_eq!(
            json::text(&page.advance(&html, false).unwrap().unwrap(), "path"),
            "locales/nl.json"
        );
        assert_eq!(
            json::text(
                &page
                    .advance(&asset("not valid JSON"), false)
                    .unwrap()
                    .unwrap(),
                "path"
            ),
            "locales/en.json"
        );
        let locale = asset(r#"{"title":"</script><script>untrusted()</script>","ok":"Settings"}"#);
        assert!(page.advance(&locale, false).unwrap().is_none());
        let response = page.response(&locale, false).unwrap();
        let body = core::str::from_utf8(response.body.bytes()).unwrap();
        assert!(body.contains("window.__STULP_LOCALE__={"));
        assert!(body.contains("\\u003c/script\\u003e"));
        assert!(!body.contains("<script>untrusted()"));
        assert!(body.contains("<body>Settings</body>"));
        assert!(body.contains("src=\"/stulp.js\""));
    }
}
