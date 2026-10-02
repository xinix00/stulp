//! Vergelijkt uitsluitend synthetische Go-apparaatfixtures met de Rust-modeltransformatie.
use std::io::{BufRead, Write};
use stulp_core::json::{self, Value};
use stulp_matter::{devices, reports};
use stulp_sdk::{Error, clone};
fn main() -> Result<(), Box<dyn std::error::Error>> {
    let stdin = std::io::stdin();
    let mut stdout = std::io::stdout().lock();
    for line in stdin.lock().lines() {
        let line = line?;
        if line.len() > 65535 {
            return Err("test fixture exceeds limit".into());
        }
        let input = json::parse(line.as_bytes())?;
        let out = process(&input)?;
        writeln!(stdout, "{}", json::to_string(&out)?)?;
        stdout.flush()?;
    }
    Ok(())
}

fn process(input: &Value) -> stulp_sdk::Result<Value> {
    if let Some(values) = json::get(input, "values") {
        let plan = stulp_matter::commands::plan(stulp_sdk::util::field(input, "device"), values)?;
        let mut commands = Vec::new();
        for command in plan.commands {
            json::push(
                &mut commands,
                json::fields(&[
                    ("timed", Value::Bool(command.command.timed)),
                    (
                        "wire",
                        clone(stulp_sdk::util::field(
                            &stulp_sdk::asset(&command.encode()?)?,
                            "data",
                        ))?,
                    ),
                    ("values", command.values),
                ])?,
                256,
            )?;
        }
        return Ok(json::fields(&[
            ("commands", Value::Array(commands)),
            ("errors", plan.errors),
        ])?);
    }
    let mut devices = Vec::new();
    for device in json::array(input, "devices") {
        json::push(&mut devices, clone(device)?, 128)?;
    }
    let result = devices::combine(devices)?;
    let paths = reports::subscription(&result.devices)?;
    let mut attributes = Vec::new();
    for path in paths.attributes {
        json::push(
            &mut attributes,
            json::fields(&[
                (
                    "endpoint",
                    Value::uint(u64::from(
                        path.endpoint
                            .ok_or(Error::Invalid("wildcard test endpoint"))?,
                    )),
                ),
                (
                    "cluster",
                    Value::uint(u64::from(
                        path.cluster
                            .ok_or(Error::Invalid("wildcard test cluster"))?,
                    )),
                ),
                (
                    "attribute",
                    Value::uint(u64::from(
                        path.attribute
                            .ok_or(Error::Invalid("wildcard test attribute"))?,
                    )),
                ),
            ])?,
            256,
        )?;
    }
    let out = json::fields(&[
        ("devices", Value::Array(result.devices)),
        ("replacements", result.replacements),
        ("attributes", Value::Array(attributes)),
    ])?;

    Ok(out)
}
