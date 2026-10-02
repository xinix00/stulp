//! Genereert alleen tijdelijke testidentiteit; stdout gaat uitsluitend naar de lokale Go-testpeer.
mod support;
use std::io::{BufRead, Write};
use stulp_matter::{
    commissioning::{Administrator, Commissioning},
    fabric::Fabric,
    im::{Command, CommandPath},
    interaction::Interaction,
    mrp::Timing,
    network::{Network, Peer},
};
use stulp_sdk::{Client, Error};
fn main() -> Result<(), Box<dyn std::error::Error>> {
    let mut address = String::new();
    std::io::stdin().lock().read_line(&mut address)?;
    let address = address.trim();
    if !address.starts_with("127.0.0.1:") {
        return Err("test requires loopback".into());
    }
    let mut c = Client::new(support::Adapter::new()?);
    hostnet::block_on(async {
        let mut fabric = Fabric::load(&mut c).await?;
        // Alleen synthetische secrets naar de Go-kindprocespipe; nooit het echte huisdocument.
        println!(
            "{}",
            stulp_core::json::to_string(stulp_sdk::util::field(c.state().root(), "appState"))
                .map_err(stulp_core::Error::from)?
        );
        std::io::stdout()
            .flush()
            .map_err(|_| Error::Transport("test pipe"))?;
        let mut n = Network::open(&mut c).await?;
        let deadline = c.now() + 20000;
        let session = n
            .pase(&mut c, address, 20202021, Timing::default(), deadline)
            .await?;
        let mut commissioner = Commissioning {
            im: Interaction {
                network: &mut n,
                address,
                session: session.session,
            },
            endpoint: 0,
        };
        commissioner.arm(&mut c, 120, 1, deadline).await?;
        commissioner.configure(&mut c, "XX", 2, deadline).await?;
        let attestation = commissioner
            .attest(&mut c, &session.challenge, 0xfff1, 0x8000, deadline)
            .await?;
        if let Some(index) = commissioner.stale(&mut c, fabric.id(), deadline).await? {
            commissioner.remove(&mut c, index, deadline).await?;
        }
        let key = commissioner
            .csr(&mut c, &attestation, &session.challenge, deadline)
            .await?;
        commissioner
            .add_root(&mut c, &fabric.root()?, deadline)
            .await?;
        let node = fabric.allocate(&mut c).await?;
        let random = c.random()?;
        let mut serial = [0; 16];
        serial.copy_from_slice(&random[..16]);
        let noc = fabric.issue(&key, node, serial, c.wall_time()?)?;
        let ipk = fabric.case().epoch_ipk();
        let index = commissioner
            .add_noc(
                &mut c,
                &noc,
                Administrator {
                    ipk: &ipk,
                    subject: fabric.case().node(),
                    vendor: 0xfff1,
                },
                deadline,
            )
            .await?
            .index()?;
        if index != 7 {
            return Err(Error::Invalid("test fabric index"));
        }
        n.remove(session.session)?;
        drop(session);
        let secure = n
            .case(
                &mut c,
                fabric.case(),
                Peer {
                    address,
                    node,
                    noc: &noc,
                    timing: Timing::default(),
                },
                deadline,
            )
            .await?;
        let mut im = Interaction {
            network: &mut n,
            address,
            session: secure,
        };
        let devices = stulp_matter::model::inspect(
            &mut im,
            &mut c,
            &stulp_matter::model::Identity {
                node,
                noc: &noc,
                fabric: index,
                address,
                vendor: 0xfff1,
                product: 0x8000,
            },
            deadline,
        )
        .await?;
        use stulp_core::json;
        if devices.len() != 1
            || json::text(&devices[0], "name") != "Fake Lamp"
            || json::text(&devices[0], "class") != "light"
            || json::array(&devices[0], "capabilities") != [json::string("onoff")?]
            || json::get(stulp_sdk::util::field(&devices[0], "state"), "onoff")
                != Some(&json::Value::Bool(false))
        {
            return Err(Error::Invalid("test device model differs"));
        }
        let inventory = json::array(
            stulp_sdk::util::field(&devices[0], "store"),
            "~matter.endpointInventory",
        );
        if inventory.len() != 1
            || json::array(&inventory[0], "clusters").len() != 1
            || json::text(&json::array(&inventory[0], "clusters")[0], "coverage") != "partial"
        {
            return Err(Error::Invalid("test cluster inventory differs"));
        }
        let mut commissioner = Commissioning { im, endpoint: 0 };
        commissioner.complete(&mut c, deadline).await?;
        control(&mut commissioner.im, &mut c, true, deadline).await?;
        n.remove(secure)?;
        // Opnieuw laden gebruikt dezelfde identiteit en een nieuwe operationele CASE.
        let reloaded = Fabric::load(&mut c).await?;
        if reloaded.case().compressed_id()? != fabric.case().compressed_id()? {
            return Err(Error::Invalid("test identity changed"));
        }
        let secure = n
            .case(
                &mut c,
                reloaded.case(),
                Peer {
                    address,
                    node,
                    noc: &noc,
                    timing: Timing::default(),
                },
                deadline,
            )
            .await?;
        let mut im = Interaction {
            network: &mut n,
            address,
            session: secure,
        };
        control(&mut im, &mut c, false, deadline).await?;
        let mut commissioner = Commissioning { im, endpoint: 0 };
        commissioner.remove(&mut c, index, deadline).await?;
        Ok::<(), Error>(())
    })?;
    Ok(())
}
async fn control(
    im: &mut Interaction<'_>,
    c: &mut Client<support::Adapter>,
    on: bool,
    deadline: u64,
) -> stulp_sdk::Result {
    let results = im
        .invoke(
            c,
            &[Command {
                path: CommandPath {
                    endpoint: 1,
                    cluster: 6,
                    command: u32::from(on),
                },
                fields: &[21, 24],
                reference: None,
            }],
            None,
            deadline,
        )
        .await?;
    for chunk in results.iter() {
        for result in chunk?.results {
            result.status.result()?;
        }
    }
    Ok(())
}
