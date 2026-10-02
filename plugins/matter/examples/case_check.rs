//! De echte UDP-werker en SDK voeren CASE/IM uit tegen de oorspronkelijke Go-stack.
mod support;
use std::io::BufRead;
use stulp_matter::{
    case, im,
    interaction::Interaction,
    mrp::{self, Timing},
    network::{Network, Peer},
    tlv,
};
use stulp_sdk::{Client, Error};
use support::Adapter;
fn decode(s: &str) -> std::result::Result<Vec<u8>, Box<dyn std::error::Error>> {
    if s.len() > 4096 || !s.len().is_multiple_of(2) {
        return Err("hex length".into());
    }
    s.as_bytes()
        .chunks_exact(2)
        .map(|b| Ok(u8::from_str_radix(std::str::from_utf8(b)?, 16)?))
        .collect()
}
fn main() -> std::result::Result<(), Box<dyn std::error::Error>> {
    let mut line = String::new();
    std::io::stdin().lock().read_line(&mut line)?;
    let fields: Vec<_> = line.split_whitespace().collect();
    if fields.len() != 6 && fields.len() != 1 {
        return Err("fixture fields".into());
    }
    let fabric = if fields.len() == 6 {
        Some(case::Fabric::new(
            0x1111,
            0x3333,
            &decode(fields[1])?,
            decode(fields[2])?.as_slice().try_into()?,
            decode(fields[3])?.as_slice().try_into()?,
            &decode(fields[4])?,
        )?)
    } else {
        None
    };
    let noc = if fields.len() == 6 {
        decode(fields[5])?
    } else {
        Vec::new()
    };
    let mut c = Client::new(Adapter::new()?);
    hostnet::block_on(async {
        let deadline = 30000;
        let mut n = Network::open(&mut c).await?;
        let session = if let Some(fabric) = &fabric {
            n.case(
                &mut c,
                fabric,
                Peer {
                    address: fields[0],
                    node: 0x4444,
                    noc: &noc,
                    timing: Timing::default(),
                },
                deadline,
            )
            .await?
        } else {
            n.pase(&mut c, fields[0], 20202021, Timing::default(), deadline)
                .await?
                .session
        };
        let mut im = Interaction {
            network: &mut n,
            address: fields[0],
            session,
        };
        let report = im
            .read(&mut c, &[im::AttributePath::new(1, 6, 0)], true, deadline)
            .await?;
        let mut count = 0;
        for report in report.iter() {
            for attribute in report?.attributes {
                count += 1;
                if attribute.path.attribute != Some(0) {
                    return Err(Error::Invalid("test attribute"));
                }
            }
        }
        if count != 2 {
            return Err(Error::Invalid("chunked read lost data"));
        }
        let commands = [im::Command {
            path: im::CommandPath {
                endpoint: 1,
                cluster: 0x101,
                command: 1,
            },
            fields: &[21, 24],
            reference: Some(9),
        }];
        let invoked = im.invoke(&mut c, &commands, Some(2000), deadline).await?;
        let mut count = 0;
        for chunk in invoked.iter() {
            for r in chunk?.results {
                r.status.result()?;
                if r.reference != Some(9) {
                    return Err(Error::Invalid("command reference lost"));
                }
                count += 1;
            }
        }
        if count != 1 {
            return Err(Error::Invalid("invoke result missing"));
        }
        let writes = [im::Write {
            path: im::AttributePath::new(1, 6, 0),
            version: None,
            value: &[9],
        }];
        let written = im.write(&mut c, &writes, deadline).await?;
        if written.len() != 1 || written[0].1.global != 0x86 {
            return Err(Error::Invalid("write rejection lost"));
        }
        let request = im::Subscription {
            attributes: vec![im::AttributePath::new(1, 6, 0)],
            events: Vec::new(),
            minimum: 0,
            maximum: 30,
            keep: false,
            fabric_filtered: true,
        };
        let subscription = im.subscribe(&mut c, &request, deadline).await?;
        if subscription.id != 123 || subscription.maximum != 30 {
            return Err(Error::Invalid("subscription differs"));
        }
        loop {
            if c.now() >= deadline {
                return Err(Error::Timeout);
            }
            n.tick(&mut c)?;
            if let Some(mrp::Event::Accepted(h)) = n.event() {
                let mut im = Interaction {
                    network: &mut n,
                    address: fields[0],
                    session,
                };
                let reports = im.report(&mut c, h, subscription.id, deadline).await?;
                for chunk in reports.iter() {
                    for attribute in chunk?.attributes {
                        if attribute.value.as_ref().map(|n| n.element.value)
                            != Some(tlv::Value::Bool(true))
                        {
                            return Err(Error::Invalid("unsolicited report value"));
                        }
                    }
                }
                break;
            }
            c.idle().await?;
        }
        Ok::<(), Error>(())
    })?;
    if c.into_transport().pings == 0 {
        return Err("SDK heartbeat did not run during slow device response".into());
    }
    Ok(())
}
