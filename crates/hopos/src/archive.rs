//! External-slot backups share the portable ZIP validator and atomic A/B store.
use alloc::vec::Vec;
use stulp_core::{
    Result, json,
    store::{Storage, Store},
};
use stulp_web::{Body, Environment, Request, Response};
pub(crate) fn route<S: Storage>(
    store: &mut Store<S>,
    request: Request,
    env: &impl Environment,
) -> Result<Response> {
    if request.method == "GET" {
        let bytes =
            stulp_controller::archive::write_external(&store.document().encode()?, &env.now()?)?;
        let mut headers = Vec::new();
        json::push(
            &mut headers,
            (
                "Content-Disposition",
                json::copy("attachment; filename=\"stulp-backup.zip\"")?,
            ),
            8,
        )?;
        return Ok(Response {
            status: 200,
            content_type: "application/zip",
            body: Body::Bytes(bytes),
            cookie: None,
            headers,
        });
    }
    let kind = json::text(&request.headers, "content-type")
        .split(';')
        .next()
        .unwrap_or("")
        .trim();
    if !kind.eq_ignore_ascii_case("application/zip")
        && !kind.eq_ignore_ascii_case("application/octet-stream")
    {
        return Response::error(415, "upload a Stulp .zip backup");
    }
    let document = stulp_controller::archive::read_external(&request.body)?;
    store.restore(document.as_bytes())?;
    Response::json(
        200,
        &json::fields(&[("restored", json::Value::Bool(true))])?,
    )
}
