//! Begrensde IM-transacties, inclusief chunks, subscription-priming en timed invokes.
use crate::{im, message::Message, mrp::Handle, network::Network};
use alloc::vec::Vec;
use stulp_core::json;
use stulp_sdk::{Client, Error, Result, Transport};
/// Ongewijzigde wirechunks bezitten hun TLV-data; iterators lenen daaruit zonder zelfreferenties.
#[derive(Default)]
pub struct Reports {
    chunks: Vec<Vec<u8>>,
    bytes: usize,
    subscription: Option<u32>,
}
impl Reports {
    /// Alle attributen en events blijven in ontvangstvolgorde, ook in arrays over meerdere chunks.
    pub fn iter(&self) -> impl Iterator<Item = Result<im::Report<'_>>> {
        self.chunks.iter().map(|b| im::Report::parse(b))
    }
    /// Subscription-ID wanneer dit een geverifieerd subscriptionrapport is.
    pub fn subscription(&self) -> Option<u32> {
        self.subscription
    }
    pub(crate) fn push(&mut self, bytes: Vec<u8>) -> Result {
        let size = bytes.len();
        if size > 65535usize.saturating_sub(self.bytes) {
            return Err(Error::Invalid("Matter report exceeds 64 KiB"));
        }
        json::push(&mut self.chunks, bytes, 64)?;
        self.bytes += size;
        Ok(())
    }
}
/// Resultaten van commands blijven beschikbaar, inclusief individuele afwijzingen.
#[derive(Default)]
pub struct Invoked {
    chunks: Vec<Vec<u8>>,
    bytes: usize,
}
impl Invoked {
    /// De caller controleert ieder commandresultaat; een geslaagd exchange is geen cluster-success.
    pub fn iter(&self) -> impl Iterator<Item = Result<im::InvokeResponse<'_>>> {
        self.chunks.iter().map(|b| im::InvokeResponse::parse(b))
    }
    fn push(&mut self, bytes: Vec<u8>) -> Result {
        let size = bytes.len();
        if size > 65535usize.saturating_sub(self.bytes) {
            return Err(Error::Invalid("Matter invoke result exceeds 64 KiB"));
        }
        json::push(&mut self.chunks, bytes, 64)?;
        self.bytes += size;
        Ok(())
    }
}
/// Geaccepteerd abonnement met geverifieerde priming-data en negotiated interval.
pub struct Subscribed {
    /// Door de peer toegewezen ID.
    pub id: u32,
    /// Maximale rapportageperiode in seconden.
    pub maximum: u16,
    /// Alle initiële waarden en events.
    pub reports: Reports,
}
/// Eén korte lening van de netwerk-eigenaar voor een beveiligd apparaat.
pub struct Interaction<'a> {
    /// Dezelfde eigenaar voor command- en subscriptionexchanges.
    pub network: &'a mut Network,
    /// Canoniek peeradres.
    pub address: &'a str,
    /// Lokaal ontvangend sessie-ID.
    pub session: u16,
}
fn expected(message: Message, opcode: u8) -> Result<Vec<u8>> {
    if message.protocol.opcode == 1 {
        im::read_status(&message.payload)?.result()?;
        return Err(Error::Invalid(
            "Matter returned status instead of requested data",
        ));
    }
    if message.protocol.opcode != opcode {
        return Err(Error::Invalid("unexpected Interaction Model opcode"));
    }
    Ok(message.payload)
}
impl Interaction<'_> {
    fn begin<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        opcode: u8,
        payload: &[u8],
    ) -> Result<Handle> {
        if self.session == 0 {
            return Err(Error::Invalid(
                "Interaction Model requires a secure session",
            ));
        }
        let h = self.network.initiate(c, self.address, self.session, 1, 0)?;
        if let Err(e) = self.network.send(c, h, opcode, payload) {
            self.network.close(h);
            return Err(e);
        }
        Ok(h)
    }
    /// Leest met expliciete fabric-filter; PASE gebruikt voor de fabric-tabel false.
    pub async fn read<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        paths: &[im::AttributePath],
        filtered: bool,
        deadline: u64,
    ) -> Result<Reports> {
        let h = self.begin(c, 2, &im::read(paths, filtered)?)?;
        let result = self.reports(c, h, ReportKind::Read, deadline).await;
        let _ = self.network.acknowledge(c, h);
        self.network.close(h);
        result
    }
    async fn reports<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        h: Handle,
        kind: ReportKind,
        deadline: u64,
    ) -> Result<Reports> {
        let mut combined = Reports::default();
        loop {
            let bytes = expected(self.network.receive(c, h, deadline).await?, 5)?;
            let report = im::Report::parse(&bytes)?;
            match kind {
                ReportKind::Read if report.subscription.is_some() => {
                    return Err(Error::Invalid(
                        "read received unsolicited subscription data",
                    ));
                }
                ReportKind::Subscribe | ReportKind::Update(_) if report.subscription.is_none() => {
                    return Err(Error::Invalid("subscription report missing ID"));
                }
                ReportKind::Update(id) if report.subscription != Some(id) => {
                    self.network.send(c, h, 1, &im::status(0x7d)?)?;
                    self.network.wait_ack(c, h, deadline).await?;
                    return Err(Error::Invalid("unknown Matter subscription ID"));
                }
                _ => (),
            }
            if combined.subscription.is_some() && combined.subscription != report.subscription {
                return Err(Error::Invalid(
                    "Matter subscription ID changed between chunks",
                ));
            }
            let more = report.more;
            let suppress = report.suppress;
            combined.subscription = report.subscription;
            combined.push(bytes)?;
            if suppress {
                self.network.acknowledge(c, h)?;
            } else {
                self.network.send(c, h, 1, &im::status(0)?)?;
                // Bij een live rapport wacht de eigenaar niet op de ACK van de
                // laatste StatusResponse: die blijft betrouwbaar (de exchange
                // blijft open tot zijn ACK, zie Engine), maar de waarden en
                // flowtriggers gaan meteen door. Tot 03-10 kostte dat wachten
                // elk rapport 550 tot 600 ms, ook een bewegingsmelding.
                if more || !matches!(kind, ReportKind::Update(_)) {
                    self.network.wait_ack(c, h, deadline).await?;
                }
            }
            if !more {
                return Ok(combined);
            }
        }
    }
    /// Sluit een abonnement af met dezelfde ID als de volledige priming-report.
    pub async fn subscribe<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        request: &im::Subscription,
        deadline: u64,
    ) -> Result<Subscribed> {
        let h = self.begin(c, 3, &request.encode()?)?;
        let result = async {
            let reports = self.reports(c, h, ReportKind::Subscribe, deadline).await?;
            let bytes = expected(self.network.receive(c, h, deadline).await?, 4)?;
            let (id, maximum) = im::subscribed(&bytes)?;
            if Some(id) != reports.subscription {
                return Err(Error::Invalid(
                    "Matter subscription parameters differ from request",
                ));
            }
            self.network.acknowledge(c, h)?;
            Ok(Subscribed {
                id,
                maximum,
                reports,
            })
        }
        .await;
        let _ = self.network.acknowledge(c, h);
        self.network.close(h);
        result
    }
    /// Volledig unsolicited rapport op een door de peer begonnen exchange; caller verifieert sessie/ID.
    pub async fn report<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        h: Handle,
        id: u32,
        deadline: u64,
    ) -> Result<Reports> {
        let (_, session, protocol, _) = self.network.peer(h)?;
        if session != self.session || protocol != 1 {
            return Err(Error::Invalid(
                "subscription arrived on another session or protocol",
            ));
        }
        let result = self.reports(c, h, ReportKind::Update(id), deadline).await;
        if result.is_err() {
            let _ = self.network.acknowledge(c, h);
            self.network.close(h);
        }
        // Bij succes blijft de exchange open tot de ACK of het opgeven van
        // MRP; de Engine sluit hem dan (Event::Acknowledged of Failed).
        result
    }
    /// Iedere attribuutstatus wordt afzonderlijk teruggegeven, zonder optimistische devicewaarden.
    pub async fn write<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        values: &[im::Write<'_>],
        deadline: u64,
    ) -> Result<Vec<(im::AttributePath, im::Status)>> {
        let h = self.begin(c, 6, &im::write(values, false)?)?;
        let result = async {
            let bytes = expected(self.network.receive(c, h, deadline).await?, 7)?;
            let results = im::write_response(&bytes)?;
            self.network.acknowledge(c, h)?;
            Ok(results)
        }
        .await;
        let _ = self.network.acknowledge(c, h);
        self.network.close(h);
        result
    }
    /// TimedRequest en InvokeRequest delen één exchange; timeouts worden nooit als nieuw command herhaald.
    pub async fn invoke<T: Transport>(
        &mut self,
        c: &mut Client<T>,
        commands: &[im::Command<'_>],
        timeout: Option<u16>,
        deadline: u64,
    ) -> Result<Invoked> {
        // Encodeer eerst; een ongeldige command mag geen timed window openen.
        let payload = im::invoke(commands, timeout.is_some())?;
        let started = c.now();
        let h = if let Some(timeout) = timeout {
            self.begin(c, 10, &im::timed(timeout)?)?
        } else {
            self.begin(c, 8, &payload)?
        };
        let result = async {
            if let Some(timeout) = timeout {
                let status = self
                    .network
                    .receive(
                        c,
                        h,
                        deadline.min(started.saturating_add(u64::from(timeout))),
                    )
                    .await?;
                if status.protocol.opcode != 1 {
                    return Err(Error::Invalid("TimedRequest did not receive status"));
                }
                im::read_status(&status.payload)?.result()?;
                if c.now() >= started.saturating_add(u64::from(timeout)) {
                    return Err(Error::Timeout);
                }
                self.network.send(c, h, 8, &payload)?;
            }
            let mut results = Invoked::default();
            loop {
                let bytes = expected(self.network.receive(c, h, deadline).await?, 9)?;
                let response = im::InvokeResponse::parse(&bytes)?;
                let more = response.more;
                results.push(bytes)?;
                if !more {
                    self.network.acknowledge(c, h)?;
                    return Ok(results);
                }
                self.network.send(c, h, 1, &im::status(0)?)?;
                self.network.wait_ack(c, h, deadline).await?;
            }
        }
        .await;
        let _ = self.network.acknowledge(c, h);
        self.network.close(h);
        result
    }
}
#[derive(Clone, Copy)]
enum ReportKind {
    Read,
    Subscribe,
    Update(u32),
}
