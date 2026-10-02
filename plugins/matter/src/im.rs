//! Interaction Model-framing; clusterwaarden blijven getypeerde, geleende TLV-bomen.
use crate::tlv::{Node, Tag, Value, Writer};
use alloc::vec::Vec;
use stulp_core::json;
use stulp_sdk::{Error, Result};
/// Wildcards zijn None; nul is een echt endpoint, cluster of attribuut.
#[derive(Clone, Copy, Default, Debug, PartialEq, Eq)]
pub struct AttributePath {
    /// Endpoint of wildcard.
    pub endpoint: Option<u16>,
    /// Cluster of wildcard.
    pub cluster: Option<u32>,
    /// Attribuut of wildcard.
    pub attribute: Option<u32>,
    /// None vervangt het hele attribuut; Some(None) voegt aan een lijst toe.
    pub list_index: Option<Option<u16>>,
}
/// Eventselectie, inclusief optionele node en urgentievlag.
#[derive(Clone, Copy, Default, Debug, PartialEq, Eq)]
pub struct EventPath {
    /// Node-ID of wildcard.
    pub node: Option<u64>,
    /// Endpoint of wildcard.
    pub endpoint: Option<u16>,
    /// Cluster of wildcard.
    pub cluster: Option<u32>,
    /// Event-ID of wildcard.
    pub event: Option<u32>,
    /// Alleen urgente events wanneer true.
    pub urgent: Option<bool>,
}
/// Commands hebben altijd een concreet pad.
#[derive(Clone, Copy, Default, Debug, PartialEq, Eq)]
pub struct CommandPath {
    /// Endpoint.
    pub endpoint: u16,
    /// Cluster.
    pub cluster: u32,
    /// Command-ID.
    pub command: u32,
}
fn optional<T: TryFrom<u64>>(n: &Node<'_>, tag: u8) -> Result<Option<T>> {
    match n.get(tag) {
        None => Ok(None),
        Some(v) => match v.element.value {
            Value::Uint(v) => T::try_from(v)
                .map(Some)
                .map_err(|_| Error::Invalid("IM unsigned field overflows")),
            _ => Err(Error::Invalid("IM field is not unsigned")),
        },
    }
}
fn uint<T: TryFrom<u64>>(n: &Node<'_>, tag: u8) -> Result<T> {
    optional(n, tag)?.ok_or(Error::Invalid("missing IM unsigned field"))
}
fn flag(n: &Node<'_>, tag: u8) -> Result<Option<bool>> {
    match n.get(tag) {
        None => Ok(None),
        Some(n) => match n.element.value {
            Value::Bool(b) => Ok(Some(b)),
            _ => Err(Error::Invalid("IM flag is not boolean")),
        },
    }
}
fn kind(n: &Node<'_>, wanted: Value<'_>) -> Result {
    if n.element.value != wanted {
        return Err(Error::Invalid("unexpected IM container type"));
    }
    Ok(())
}
fn take<'a>(n: &mut Node<'a>, tag: u8) -> Result<Node<'a>> {
    n.take(tag).ok_or(Error::Invalid("missing IM field"))
}
fn child<'a, 'b>(n: &'b Node<'a>, tag: u8) -> Result<&'b Node<'a>> {
    n.get(tag).ok_or(Error::Invalid("missing IM field"))
}
fn root(bytes: &[u8]) -> Result<Node<'_>> {
    let n = Node::parse(bytes)?;
    kind(&n, Value::Structure)?;
    if n.element.tag != Tag::Anonymous {
        return Err(Error::Invalid("IM root must be anonymous"));
    }
    // De peer vermeldt zijn eigen revisie; Go en CHIP eisen hier geen gelijkheid aan onze 1.
    // Blijf het veldtype controleren, terwijl de concrete berichtvelden de compatibiliteit bepalen.
    let _revision = optional::<u8>(&n, 255)?;
    Ok(n)
}
fn start() -> Result<Writer> {
    let mut w = Writer::default();
    w.start(Tag::Anonymous, Value::Structure)?;
    Ok(w)
}
fn finish(mut w: Writer) -> Result<Vec<u8>> {
    w.uint(Tag::Context(255), 1)?;
    w.end()?;
    w.finish()
}
fn opt<T: Into<u64>>(w: &mut Writer, tag: u8, value: Option<T>) -> Result {
    if let Some(v) = value {
        w.uint(Tag::Context(tag), v.into())?;
    }
    Ok(())
}
fn flags(n: &Node<'_>, suppress: u8, more: u8) -> Result<(bool, bool)> {
    let suppress = flag(n, suppress)?.unwrap_or(false);
    let more = flag(n, more)?.unwrap_or(false);
    if suppress && more {
        return Err(Error::Invalid(
            "IM cannot suppress a required chunk response",
        ));
    }
    Ok((suppress, more))
}
impl AttributePath {
    /// Concreet selectiepad.
    pub fn new(endpoint: u16, cluster: u32, attribute: u32) -> Self {
        Self {
            endpoint: Some(endpoint),
            cluster: Some(cluster),
            attribute: Some(attribute),
            list_index: None,
        }
    }
    fn concrete(&self) -> Result {
        if self.endpoint.is_none() || self.cluster.is_none() || self.attribute.is_none() {
            return Err(Error::Invalid("IM requires a concrete attribute path"));
        }
        Ok(())
    }
    fn encode(&self, w: &mut Writer, tag: Tag) -> Result {
        w.start(tag, Value::List)?;
        opt(w, 2, self.endpoint)?;
        opt(w, 3, self.cluster)?;
        opt(w, 4, self.attribute)?;
        if let Some(index) = self.list_index {
            match index {
                Some(index) => w.uint(Tag::Context(5), u64::from(index))?,
                None => w.null(Tag::Context(5))?,
            }
        }
        w.end()
    }
    fn parse(n: &Node<'_>) -> Result<Self> {
        kind(n, Value::List)?;
        unique_path(n)?;
        Ok(Self {
            endpoint: optional(n, 2)?,
            cluster: optional(n, 3)?,
            attribute: optional(n, 4)?,
            list_index: match n.get(5) {
                None => None,
                Some(v) if v.element.value == Value::Null => Some(None),
                Some(_) => Some(optional(n, 5)?),
            },
        })
    }
}
impl EventPath {
    fn encode(&self, w: &mut Writer, tag: Tag) -> Result {
        w.start(tag, Value::List)?;
        opt(w, 0, self.node)?;
        opt(w, 1, self.endpoint)?;
        opt(w, 2, self.cluster)?;
        opt(w, 3, self.event)?;
        if let Some(v) = self.urgent {
            w.boolean(Tag::Context(4), v)?;
        }
        w.end()
    }
    fn parse(n: &Node<'_>) -> Result<Self> {
        kind(n, Value::List)?;
        unique_path(n)?;
        Ok(Self {
            node: optional(n, 0)?,
            endpoint: optional(n, 1)?,
            cluster: optional(n, 2)?,
            event: optional(n, 3)?,
            urgent: flag(n, 4)?,
        })
    }
}
fn unique_path(n: &Node<'_>) -> Result {
    for (at, child) in n.children.iter().enumerate() {
        if n.children[..at]
            .iter()
            .any(|prior| prior.element.tag == child.element.tag)
        {
            return Err(Error::Invalid("duplicate IM path field"));
        }
    }
    Ok(())
}
impl CommandPath {
    fn encode(&self, w: &mut Writer, tag: Tag) -> Result {
        w.start(tag, Value::List)?;
        w.uint(Tag::Context(0), u64::from(self.endpoint))?;
        w.uint(Tag::Context(1), u64::from(self.cluster))?;
        w.uint(Tag::Context(2), u64::from(self.command))?;
        w.end()
    }
    fn parse(n: &Node<'_>) -> Result<Self> {
        kind(n, Value::List)?;
        unique_path(n)?;
        Ok(Self {
            endpoint: uint(n, 0)?,
            cluster: uint(n, 1)?,
            command: uint(n, 2)?,
        })
    }
}
/// Globale en eventuele clusterspecifieke status blijven afzonderlijk.
#[derive(Clone, Copy, Default, Debug, PartialEq, Eq)]
pub struct Status {
    /// Nul betekent success.
    pub global: u8,
    /// Clusterspecifieke reden.
    pub cluster: Option<u8>,
}
impl Status {
    fn parse(n: &Node<'_>) -> Result<Self> {
        kind(n, Value::Structure)?;
        Ok(Self {
            global: uint(n, 0)?,
            cluster: optional(n, 1)?,
        })
    }
    fn encode(&self, w: &mut Writer, tag: Tag) -> Result {
        w.start(tag, Value::Structure)?;
        w.uint(Tag::Context(0), u64::from(self.global))?;
        opt(w, 1, self.cluster)?;
        w.end()
    }
    /// Geeft een protocolfout door zonder hem tot een transportfout te reduceren.
    pub fn result(self) -> Result {
        if self.global == 0 {
            Ok(())
        } else {
            Err(Error::Remote(stulp_sdk::util::join(&[
                "Matter IM status ",
                &json::to_string(&json::Value::uint(u64::from(self.global)))
                    .map_err(stulp_core::Error::from)?,
            ])?))
        }
    }
}
/// ReadRequest met minstens één pad.
pub fn read(paths: &[AttributePath], fabric_filtered: bool) -> Result<Vec<u8>> {
    if paths.is_empty() || paths.len() > 256 {
        return Err(Error::Invalid("IM read needs 1..256 paths"));
    }
    let mut w = start()?;
    w.start(Tag::Context(0), Value::Array)?;
    for path in paths {
        path.encode(&mut w, Tag::Anonymous)?;
    }
    w.end()?;
    w.boolean(Tag::Context(3), fabric_filtered)?;
    finish(w)
}
/// Subscription-opbouw gebruikt dezelfde selectors voor priming en latere reports.
pub struct Subscription {
    /// Attribuutselectors.
    pub attributes: Vec<AttributePath>,
    /// Eventselectors.
    pub events: Vec<EventPath>,
    /// Minimale rapportage-interval in seconden.
    pub minimum: u16,
    /// Maximale rapportage-interval in seconden.
    pub maximum: u16,
    /// Bestaande subscriptions binnen deze fabric behouden.
    pub keep: bool,
    /// Alleen gegevens van de eigen fabric.
    pub fabric_filtered: bool,
}
impl Subscription {
    /// Encodeert SubscribeRequest inclusief verplichte controlevelden.
    pub fn encode(&self) -> Result<Vec<u8>> {
        if self.attributes.len() + self.events.len() == 0
            || self.attributes.len() + self.events.len() > 256
            || self.maximum == 0
            || self.minimum > self.maximum
        {
            return Err(Error::Invalid("invalid IM subscription paths or interval"));
        }
        let mut w = start()?;
        w.boolean(Tag::Context(0), self.keep)?;
        w.uint_width(Tag::Context(1), u64::from(self.minimum), 2)?;
        w.uint_width(Tag::Context(2), u64::from(self.maximum), 2)?;
        if !self.attributes.is_empty() {
            w.start(Tag::Context(3), Value::Array)?;
            for p in &self.attributes {
                p.encode(&mut w, Tag::Anonymous)?;
            }
            w.end()?;
        }
        if !self.events.is_empty() {
            w.start(Tag::Context(4), Value::Array)?;
            for p in &self.events {
                p.encode(&mut w, Tag::Anonymous)?;
            }
            w.end()?;
        }
        w.boolean(Tag::Context(7), self.fabric_filtered)?;
        finish(w)
    }
}
/// Eén command met cluster-eigen velden als een TLV-structure.
pub struct Command<'a> {
    /// Concreet commandpad.
    pub path: CommandPath,
    /// Anonieme structure, omgetagd naar het commandveld.
    pub fields: &'a [u8],
    /// Optioneel referentienummer bij meerdere commands.
    pub reference: Option<u16>,
}
/// Encodeert InvokeRequest; veilig gevoelige clusters gebruiken eerst TimedRequest.
pub fn invoke(commands: &[Command<'_>], timed: bool) -> Result<Vec<u8>> {
    if commands.is_empty() || commands.len() > 256 {
        return Err(Error::Invalid("IM invoke needs 1..256 commands"));
    }
    let mut w = start()?;
    w.boolean(Tag::Context(0), false)?;
    w.boolean(Tag::Context(1), timed)?;
    w.start(Tag::Context(2), Value::Array)?;
    for cmd in commands {
        w.start(Tag::Anonymous, Value::Structure)?;
        cmd.path.encode(&mut w, Tag::Context(0))?;
        let n = Node::parse(cmd.fields)?;
        kind(&n, Value::Structure)?;
        w.node(&n, Tag::Context(1))?;
        opt(&mut w, 2, cmd.reference)?;
        w.end()?;
    }
    w.end()?;
    finish(w)
}
/// Bepaalt het venster voor de volgende write/invoke op dezelfde exchange.
pub fn timed(timeout: u16) -> Result<Vec<u8>> {
    if timeout == 0 {
        return Err(Error::Invalid("IM timed request needs a nonzero timeout"));
    }
    let mut w = start()?;
    w.uint_width(Tag::Context(0), u64::from(timeout), 2)?;
    finish(w)
}
/// Eén concrete attribuutwrite met een cluster-eigen TLV-waarde.
pub struct Write<'a> {
    /// Concreet attribuutpad.
    pub path: AttributePath,
    /// Voorwaardelijke versie.
    pub version: Option<u32>,
    /// Eén volledige TLV-waarde, inclusief type.
    pub value: &'a [u8],
}
/// Encodeert WriteRequest, zonder optimistische wijziging van de apparaatstaat.
pub fn write(writes: &[Write<'_>], timed: bool) -> Result<Vec<u8>> {
    if writes.is_empty() || writes.len() > 256 {
        return Err(Error::Invalid("IM write needs 1..256 values"));
    }
    let mut w = start()?;
    w.boolean(Tag::Context(0), false)?;
    w.boolean(Tag::Context(1), timed)?;
    w.start(Tag::Context(2), Value::Array)?;
    for item in writes {
        item.path.concrete()?;
        w.start(Tag::Anonymous, Value::Structure)?;
        opt(&mut w, 0, item.version)?;
        item.path.encode(&mut w, Tag::Context(1))?;
        w.node(&Node::parse(item.value)?, Tag::Context(2))?;
        w.end()?;
    }
    w.end()?;
    finish(w)
}
/// Stuurt een globale IM-status, bijvoorbeeld om een reportchunk te bevestigen.
pub fn status(code: u8) -> Result<Vec<u8>> {
    let mut w = start()?;
    w.uint(Tag::Context(0), u64::from(code))?;
    finish(w)
}
/// Decodeert een StatusResponse zonder onbekende status tot succes te maken.
pub fn read_status(bytes: &[u8]) -> Result<Status> {
    Ok(Status {
        global: uint(&root(bytes)?, 0)?,
        cluster: None,
    })
}
/// Een geaccepteerde subscription noemt ID en maximale interval.
pub fn subscribed(bytes: &[u8]) -> Result<(u32, u16)> {
    let n = root(bytes)?;
    let id = uint(&n, 0)?;
    let max = uint(&n, 2)?;
    if max == 0 {
        return Err(Error::Invalid("subscription maximum interval is zero"));
    }
    Ok((id, max))
}
/// Waarde en data-versie, of een expliciete status op hetzelfde pad.
pub struct Attribute<'a> {
    /// Gerelateerd pad.
    pub path: AttributePath,
    /// Data-versie wanneer gemeld.
    pub version: Option<u32>,
    /// Getypeerde waarneming; ontbreekt alleen bij een statusrapport.
    pub value: Option<Node<'a>>,
    /// Fout of succes zonder data.
    pub status: Option<Status>,
}
/// Eén event met precies één tijdreferentie.
pub struct Event<'a> {
    /// Gerelateerd eventpad.
    pub path: EventPath,
    /// Eventnummer voor deduplicatie, zonder floatconversie.
    pub number: u64,
    /// Prioriteit van het event.
    pub priority: u8,
    /// Tag 3–6 en waarde, voor epoch/system/delta-varianten.
    pub timestamp: Option<(u8, u64)>,
    /// Getypeerde eventdata.
    pub value: Option<Node<'a>>,
    /// Status-only eventantwoord.
    pub status: Option<Status>,
}
/// Eén ReportData-chunk; de client controleert ID en chunkvolgorde.
pub struct Report<'a> {
    /// Subscription-ID, afwezig bij een Read.
    pub subscription: Option<u32>,
    /// Attribuutwaarnemingen.
    pub attributes: Vec<Attribute<'a>>,
    /// Eventwaarnemingen.
    pub events: Vec<Event<'a>>,
    /// Geen IM-status nodig wanneer true.
    pub suppress: bool,
    /// Er volgen nog chunks op dezelfde exchange.
    pub more: bool,
}
fn attribute<'a>(mut raw: Node<'a>) -> Result<Attribute<'a>> {
    kind(&raw, Value::Structure)?;
    if raw.get(0).is_some() && raw.get(1).is_some() {
        return Err(Error::Invalid("attribute report has both data and status"));
    }
    if let Some(s) = raw.take(0) {
        kind(&s, Value::Structure)?;
        return Ok(Attribute {
            path: AttributePath::parse(child(&s, 0)?)?,
            version: None,
            value: None,
            status: Some(Status::parse(child(&s, 1)?)?),
        });
    }
    let mut data = take(&mut raw, 1)?;
    kind(&data, Value::Structure)?;
    Ok(Attribute {
        path: AttributePath::parse(child(&data, 1)?)?,
        version: optional(&data, 0)?,
        value: Some(take(&mut data, 2)?),
        status: None,
    })
}
fn event<'a>(mut raw: Node<'a>) -> Result<Event<'a>> {
    kind(&raw, Value::Structure)?;
    if raw.get(0).is_some() && raw.get(1).is_some() {
        return Err(Error::Invalid("event report has both data and status"));
    }
    if let Some(s) = raw.take(0) {
        kind(&s, Value::Structure)?;
        return Ok(Event {
            path: EventPath::parse(child(&s, 0)?)?,
            number: 0,
            priority: 0,
            timestamp: None,
            value: None,
            status: Some(Status::parse(child(&s, 1)?)?),
        });
    }
    let mut data = take(&mut raw, 1)?;
    kind(&data, Value::Structure)?;
    let mut timestamp = None;
    for tag in 3..=6 {
        if let Some(n) = optional(&data, tag)? {
            if timestamp.is_some() {
                return Err(Error::Invalid("event has multiple timestamps"));
            }
            timestamp = Some((tag, n));
        }
    }
    if timestamp.is_none() {
        return Err(Error::Invalid("event has no timestamp"));
    }
    Ok(Event {
        path: EventPath::parse(child(&data, 0)?)?,
        number: uint(&data, 1)?,
        priority: uint(&data, 2)?,
        timestamp,
        value: Some(take(&mut data, 7)?),
        status: None,
    })
}
impl<'a> Report<'a> {
    /// Behoudt de types en 64-bit eventnummers; ontbrekend is nooit gelijk aan nul.
    pub fn parse(bytes: &'a [u8]) -> Result<Self> {
        let mut n = root(bytes)?;
        let (suppress, more) = flags(&n, 4, 3)?;
        let mut out = Self {
            subscription: optional(&n, 0)?,
            attributes: Vec::new(),
            events: Vec::new(),
            suppress,
            more,
        };
        if let Some(a) = n.take(1) {
            kind(&a, Value::Array)?;
            for v in a.children {
                json::push(&mut out.attributes, attribute(v)?, 4096)?;
            }
        }
        if let Some(a) = n.take(2) {
            kind(&a, Value::Array)?;
            for v in a.children {
                json::push(&mut out.events, event(v)?, 4096)?;
            }
        }
        Ok(out)
    }
}
/// Eén commandantwoord, met optionele velden of een expliciete status.
pub struct InvokeResult<'a> {
    /// Commandpad, inclusief response-command-ID wanneer van toepassing.
    pub path: CommandPath,
    /// Responsevelden, afwezig bij een status-only antwoord.
    pub fields: Option<Node<'a>>,
    /// Nul bij commanddata, anders de gerapporteerde status.
    pub status: Status,
    /// Het referentienummer van de request.
    pub reference: Option<u16>,
}
/// Eén InvokeResponse-chunk.
pub struct InvokeResponse<'a> {
    /// Antwoorden in wirevolgorde.
    pub results: Vec<InvokeResult<'a>>,
    /// Geen IM-statusantwoord gevraagd.
    pub suppress: bool,
    /// Meer antwoorden volgen.
    pub more: bool,
}
impl<'a> InvokeResponse<'a> {
    /// Command- en statusantwoorden zijn verschillende vormen, geen stille fallback.
    pub fn parse(bytes: &'a [u8]) -> Result<Self> {
        let mut n = root(bytes)?;
        let (suppress, more) = flags(&n, 0, 2)?;
        let responses = take(&mut n, 1)?;
        kind(&responses, Value::Array)?;
        let mut results = Vec::new();
        for mut raw in responses.children {
            kind(&raw, Value::Structure)?;
            if raw.get(0).is_some() && raw.get(1).is_some() {
                return Err(Error::Invalid("invoke response has both data and status"));
            }
            let data = raw.get(0).is_some();
            let mut answer = take(&mut raw, if data { 0 } else { 1 })?;
            kind(&answer, Value::Structure)?;
            let path = CommandPath::parse(child(&answer, 0)?)?;
            let reference = optional(&answer, 2)?;
            let status = if data {
                Status::default()
            } else {
                Status::parse(child(&answer, 1)?)?
            };
            let fields = if data { answer.take(1) } else { None };
            if let Some(n) = &fields {
                kind(n, Value::Structure)?;
            }
            json::push(
                &mut results,
                InvokeResult {
                    path,
                    fields,
                    status,
                    reference,
                },
                4096,
            )?;
        }
        Ok(Self {
            results,
            suppress,
            more,
        })
    }
}
/// Leest iedere WriteResponse-status apart; een gedeeltelijke fout blijft zichtbaar.
pub fn write_response(bytes: &[u8]) -> Result<Vec<(AttributePath, Status)>> {
    let mut n = root(bytes)?;
    let statuses = take(&mut n, 0)?;
    kind(&statuses, Value::Array)?;
    let mut out = Vec::new();
    for s in statuses.children {
        kind(&s, Value::Structure)?;
        json::push(
            &mut out,
            (
                AttributePath::parse(child(&s, 0)?)?,
                Status::parse(child(&s, 1)?)?,
            ),
            4096,
        )?;
    }
    Ok(out)
}
impl Report<'_> {
    /// Encodeert reports voor protocoltests en eventuele lokale Matter-endpoints.
    pub fn encode(&self) -> Result<Vec<u8>> {
        if self.suppress && self.more {
            return Err(Error::Invalid("report cannot suppress chunk confirmation"));
        }
        let mut w = start()?;
        if let Some(id) = self.subscription {
            w.uint_width(Tag::Context(0), u64::from(id), 4)?;
        }
        if !self.attributes.is_empty() {
            w.start(Tag::Context(1), Value::Array)?;
            for a in &self.attributes {
                w.start(Tag::Anonymous, Value::Structure)?;
                if let Some(status) = a.status {
                    if a.value.is_some() {
                        return Err(Error::Invalid("attribute has both status and value"));
                    }
                    w.start(Tag::Context(0), Value::Structure)?;
                    a.path.encode(&mut w, Tag::Context(0))?;
                    status.encode(&mut w, Tag::Context(1))?;
                } else {
                    a.path.concrete()?;
                    let n = a
                        .value
                        .as_ref()
                        .ok_or(Error::Invalid("attribute report value missing"))?;
                    w.start(Tag::Context(1), Value::Structure)?;
                    opt(&mut w, 0, a.version)?;
                    a.path.encode(&mut w, Tag::Context(1))?;
                    w.node(n, Tag::Context(2))?;
                }
                w.end()?;
                w.end()?;
            }
            w.end()?;
        }
        if !self.events.is_empty() {
            w.start(Tag::Context(2), Value::Array)?;
            for e in &self.events {
                w.start(Tag::Anonymous, Value::Structure)?;
                if let Some(status) = e.status {
                    if e.value.is_some() {
                        return Err(Error::Invalid("event has both status and value"));
                    }
                    w.start(Tag::Context(0), Value::Structure)?;
                    e.path.encode(&mut w, Tag::Context(0))?;
                    status.encode(&mut w, Tag::Context(1))?;
                } else {
                    if e.path.endpoint.is_none()
                        || e.path.cluster.is_none()
                        || e.path.event.is_none()
                    {
                        return Err(Error::Invalid("event report needs a concrete path"));
                    }
                    let n = e
                        .value
                        .as_ref()
                        .ok_or(Error::Invalid("event value missing"))?;
                    let (tag, time) = e
                        .timestamp
                        .ok_or(Error::Invalid("event timestamp missing"))?;
                    if !(3..=6).contains(&tag) {
                        return Err(Error::Invalid("invalid event timestamp tag"));
                    }
                    w.start(Tag::Context(1), Value::Structure)?;
                    e.path.encode(&mut w, Tag::Context(0))?;
                    w.uint(Tag::Context(1), e.number)?;
                    w.uint_width(Tag::Context(2), u64::from(e.priority), 1)?;
                    w.uint(Tag::Context(tag), time)?;
                    w.node(n, Tag::Context(7))?;
                }
                w.end()?;
                w.end()?;
            }
            w.end()?;
        }
        if self.more {
            w.boolean(Tag::Context(3), true)?;
        }
        if self.suppress {
            w.boolean(Tag::Context(4), true)?;
        }
        finish(w)
    }
}
impl InvokeResponse<'_> {
    /// Behoudt referenties bij zowel commanddata als status-only antwoorden.
    pub fn encode(&self) -> Result<Vec<u8>> {
        if self.results.is_empty() || self.suppress && self.more {
            return Err(Error::Invalid("invalid invoke response"));
        }
        let mut w = start()?;
        w.boolean(Tag::Context(0), self.suppress)?;
        w.start(Tag::Context(1), Value::Array)?;
        for r in &self.results {
            w.start(Tag::Anonymous, Value::Structure)?;
            w.start(
                Tag::Context(if r.fields.is_some() { 0 } else { 1 }),
                Value::Structure,
            )?;
            r.path.encode(&mut w, Tag::Context(0))?;
            if let Some(n) = &r.fields {
                kind(n, Value::Structure)?;
                if r.status.global != 0 {
                    return Err(Error::Invalid("invoke data cannot carry failed status"));
                }
                w.node(n, Tag::Context(1))?;
            } else {
                r.status.encode(&mut w, Tag::Context(1))?;
            }
            opt(&mut w, 2, r.reference)?;
            w.end()?;
            w.end()?;
        }
        w.end()?;
        if self.more {
            w.boolean(Tag::Context(2), true)?;
        }
        finish(w)
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn original_go_report_preserves_signed_measurement_event_counter_and_chunk_flags() -> Result {
        let bytes = include_bytes!("../tests/fixtures/report.bin");
        let report = Report::parse(bytes)?;
        assert_eq!(report.subscription, Some(123));
        assert!(report.more);
        assert!(!report.suppress);
        assert_eq!(report.attributes.len(), 2);
        assert_eq!(report.attributes[0].version, Some(17));
        assert!(matches!(
            report.attributes[0].value.as_ref().map(|n| n.element.value),
            Some(Value::Bool(false))
        ));
        assert!(matches!(
            report.attributes[1].value.as_ref().map(|n| n.element.value),
            Some(Value::Int(-125))
        ));
        assert_eq!(report.events[0].number, 0x20000000000001);
        assert_eq!(report.events[0].timestamp, Some((4, 42)));
        assert_eq!(report.encode()?, bytes);
        let mut bad = report;
        bad.suppress = true;
        assert!(bad.encode().is_err());
        Ok(())
    }
    #[test]
    fn original_go_invoke_status_reference_and_command_fields_roundtrip() -> Result {
        let bytes = include_bytes!("../tests/fixtures/invoke.bin");
        let response = InvokeResponse::parse(bytes)?;
        assert_eq!(response.results.len(), 2);
        assert_eq!(response.results[0].reference, Some(9));
        assert!(response.results[0].fields.is_none());
        response.results[0].status.result()?;
        assert_eq!(response.results[1].path.cluster, 0x30);
        assert_eq!(
            response.results[1]
                .fields
                .as_ref()
                .and_then(|n| n.get(1))
                .map(|n| n.element.value),
            Some(Value::String("complete"))
        );
        assert_eq!(response.encode()?, bytes);
        Ok(())
    }
    #[test]
    fn peer_revisions_preserve_reports_and_commands_without_accepting_bad_field_types() -> Result {
        for revision in [1, 2, 10, 11, 12, 255] {
            let mut report = include_bytes!("../tests/fixtures/report.bin").to_vec();
            let end = report.len();
            assert_eq!(&report[end - 4..], &[0x24, 255, 1, 0x18]);
            report[end - 2] = revision;
            let parsed = Report::parse(&report)?;
            assert_eq!(parsed.subscription, Some(123));
            assert_eq!(parsed.attributes.len(), 2);
            assert_eq!(parsed.events[0].number, 0x20000000000001);

            let mut invoke = include_bytes!("../tests/fixtures/invoke.bin").to_vec();
            let end = invoke.len();
            assert_eq!(&invoke[end - 4..], &[0x24, 255, 1, 0x18]);
            invoke[end - 2] = revision;
            let parsed = InvokeResponse::parse(&invoke)?;
            assert_eq!(parsed.results.len(), 2);
            parsed.results[0].status.result()?;
        }
        assert!(root(&[21, 0x29, 255, 24]).is_err());
        assert!(root(&[21, 0x25, 255, 0, 1, 24]).is_err());
        Ok(())
    }
    #[test]
    fn requests_use_distinct_path_tags_and_require_timed_window() -> Result {
        let request = read(&[AttributePath::new(0, 0x1d, 0)], true)?;
        let r = root(&request)?;
        let paths = child(&r, 0)?;
        assert_eq!(paths.children[0].uint(2)?, 0);
        assert_eq!(paths.children[0].uint(3)?, 0x1d);
        assert_eq!(flag(&r, 3)?, Some(true));
        let fields = [21, 24];
        let path = CommandPath {
            endpoint: 1,
            cluster: 0x101,
            command: 0,
        };
        let wire = invoke(
            &[Command {
                path,
                fields: &fields,
                reference: Some(7),
            }],
            true,
        )?;
        let n = root(&wire)?;
        assert_eq!(flag(&n, 1)?, Some(true));
        let command = &child(&n, 2)?.children[0];
        assert_eq!(CommandPath::parse(child(command, 0)?)?, path);
        assert_eq!(command.uint(2)?, 7);
        assert!(timed(0).is_err());
        assert!(
            write(
                &[Write {
                    path: AttributePath::default(),
                    version: None,
                    value: &[9]
                }],
                false
            )
            .is_err()
        );
        assert!(root(&[21, 0x24, 255, 2, 24]).is_ok());
        Ok(())
    }
}
