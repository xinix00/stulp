//! Validate plugin media descriptions before opening an upstream connection.
use alloc::vec::Vec;
use stulp_core::{
    Error, Result,
    json::{self, Value},
};
use stulp_web::{Body, Response};
/// Validate a callback and prepare a bounded proxy response.
pub fn response(value: &Value, owner: u64, image: bool) -> Result<Response> {
    let url = json::text(value, "url");
    let mime = json::text(value, "contentType");
    if !(url.starts_with("http://") || url.starts_with("https://"))
        || url.len() > 4096
        || url.bytes().any(|b| b <= 32 || b == 127)
    {
        return Err(Error::Invalid("app supplied an invalid HTTP media address"));
    }
    if (image && !mime.starts_with("image/"))
        || !(mime.starts_with("video/") || mime.starts_with("image/"))
        || mime.len() > 128
        || mime.bytes().any(|b| b < 32 || b == 127)
    {
        return Err(Error::Invalid("app supplied an invalid media content type"));
    }
    Ok(Response {
        status: 200,
        content_type: "application/octet-stream",
        body: Body::Proxy {
            url: json::copy(url)?,
            mime: json::copy(mime)?,
            owner,
        },
        cookie: None,
        headers: Vec::new(),
    })
}
