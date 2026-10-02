//! Eén eigenaar, kandidaten vóór opslag en een begrensd gebeurtenissenlog.
use crate::{
    Error, Result,
    document::{Document, MAX_RECORDS},
    json::{self, TryClone, Value},
};
use alloc::{string::String, vec::Vec};

mod consolidation;
mod groups;
mod media;
mod references;
mod scenes;
mod triggers;
pub use triggers::MAX_TRIGGERS;

/// Hoogstens zoveel gebeurtenissen kunnen achterblijven op een langzame lezer.
pub const EVENT_CAPACITY: usize = 64;

/// De platformlaag garandeert atomair vervangen en duurzame bytes vóór succes.
pub trait Storage {
    /// Optioneel duurzaam documentpad voor bundel- en archiefbeheer.
    fn path(&self) -> Option<&str> {
        None
    }
    /// Publiceert één volledig document; bij een fout blijft het vorige geldig.
    fn save(&mut self, document: &[u8]) -> Result;
}

/// Bewust vluchtige opslag, uitsluitend wanneer de gebruiker daarvoor kiest.
pub struct Memory;
impl Storage for Memory {
    fn save(&mut self, _: &[u8]) -> Result {
        Ok(())
    }
}

/// Een invalidering met oplopende cursor, pas na een geslaagde mutatie.
pub struct Event {
    /// Uniek binnen deze controllerlevensduur.
    pub sequence: u64,
    /// Managernaam volgens de bestaande browser-API.
    pub manager: &'static str,
    /// Bestaand gebeurtenistype.
    pub kind: &'static str,
    /// Gewijzigde identiteit.
    pub id: String,
}

struct Observation {
    id: String,
    state: Value,
    revisions: Value,
    available: bool,
    message: String,
}

struct AppInfo {
    id: String,
    manifest: Value,
    state: &'static str,
    details: Value,
}

/// Geen mutex: alleen de controller-taak kan muteren.
pub struct Store<S> {
    document: Document,
    language: String,
    timezone: String,
    clock_ms: u64,
    statistics: crate::stats::Statistics,
    statistics_fault: Option<Error>,
    statistics_reported: bool,
    storage: S,
    live: Vec<Observation>,
    events: Vec<Event>,
    sequence: u64,
    apps: Vec<AppInfo>,
    caches: Vec<(String, Value)>,
    triggers: Vec<Value>,
    scene_trips: Vec<Value>,
    media: Vec<(String, Value)>,
    images: Vec<media::SharedImage>,
}

impl<S: Storage> Store<S> {
    /// Opent een reeds gelezen document; veroorzaakt geen schrijfactie.
    pub fn open(bytes: &[u8], storage: S) -> Result<Self> {
        let mut document = Document::decode(bytes)?;
        scenes::reconcile(&mut document)?;
        let mut events = Vec::new();
        events
            .try_reserve_exact(EVENT_CAPACITY)
            .map_err(|_| Error::Memory)?;
        Ok(Self {
            document,
            language: json::copy("nl")?,
            timezone: json::copy("UTC")?,
            clock_ms: 0,
            statistics: Default::default(),
            statistics_fault: None,
            statistics_reported: false,
            storage,
            live: Vec::new(),
            events,
            sequence: 0,
            apps: Vec::new(),
            caches: Vec::new(),
            triggers: Vec::new(),
            scene_trips: Vec::new(),
            media: Vec::new(),
            images: Vec::new(),
        })
    }

    /// De adapter zet de klok vóór een ronde appmutaties; statistiek pollt geen apparaten.
    pub fn tick(&mut self, unix_ms: u64) {
        self.clock_ms = unix_ms;
        if self.statistics_enabled() {
            self.statistics.tick(unix_ms);
        }
    }
    /// De gebruiker kiest zelf of statistiek geheugen mag gebruiken.
    pub fn statistics_enabled(&self) -> bool {
        json::get(self.document.root(), "system").is_some_and(|v| json::boolean(v, "statistics"))
    }
    /// Read-only toegang tot vluchtige meetreeksen.
    pub fn statistics(&self) -> &crate::stats::Statistics {
        &self.statistics
    }
    /// Een degradatie wordt eenmaal gemeld; apparaatbesturing blijft beschikbaar.
    pub fn statistics_fault(&mut self) -> Option<Error> {
        self.statistics_fault.take()
    }
    fn collect(&mut self, id: &str, name: &str, state: &Value) {
        if self.statistics_enabled()
            && let Err(e) = self.statistics.observe(id, name, state, self.clock_ms)
            && !self.statistics_reported
        {
            self.statistics_fault = Some(e);
            self.statistics_reported = true;
        }
    }

    /// Bundels en backups worden naast dit document geplaatst; geheugenopslag heeft geen pad.
    pub fn path(&self) -> Option<&str> {
        self.storage.path()
    }

    /// Procesopties horen niet in het duurzame huisdocument.
    pub fn configure(&mut self, language: &str, timezone: &str) -> Result {
        let language = json::copy(language)?;
        let timezone = json::copy(timezone)?;
        self.language = language;
        self.timezone = timezone;
        Ok(())
    }
    /// Gekozen taal voor appcontext en manifestteksten.
    pub fn language(&self) -> &str {
        &self.language
    }
    /// IANA-tijdzone voor de Flow-klok en apps.
    pub fn timezone(&self) -> &str {
        &self.timezone
    }

    /// Alleen de eigenaar heeft toegang tot het geheime configuratiedocument.
    pub fn document(&self) -> &Document {
        &self.document
    }

    /// Het geplaatste proces levert het manifest; een oude schijfkopie overschrijft het niet.
    pub fn announce(&mut self, id: &str, manifest: Value) -> Result {
        crate::manifest::validate(&manifest)?;
        if json::text(&manifest, "id") != id {
            return Err(Error::Invalid("manifest belongs to another app"));
        }
        let event = self.event("apps", "app.update", id)?;
        if let Some(app) = self.apps.iter_mut().find(|a| a.id == id) {
            app.manifest = manifest;
            app.state = "starting";
        } else {
            json::push(
                &mut self.apps,
                AppInfo {
                    id: json::copy(id)?,
                    manifest,
                    state: "starting",
                    details: json::object(),
                },
                MAX_RECORDS,
            )?;
        }
        self.publish(event);
        Ok(())
    }

    /// Alleen appmetadata, geen appgeheimen.
    pub fn manifest(&self, id: &str) -> Option<&Value> {
        self.apps
            .iter()
            .find(|a| a.id == id)
            .map(|a| &a.manifest)
            .filter(|m| !m.is_null())
    }

    /// Runtime-status wordt nooit naar het configuratiebestand geschreven.
    pub fn app_status(&self, id: &str) -> &str {
        self.apps
            .iter()
            .find(|a| a.id == id)
            .map(|a| a.state)
            .unwrap_or("stopped")
    }

    /// Eén statusgebeurtenis per overgang.
    pub fn set_app_status(&mut self, id: &str, state: &'static str) -> Result {
        if self.app_status(id) == state {
            return Ok(());
        }
        let event = self.event("apps", "app.update", id)?;
        if let Some(app) = self.apps.iter_mut().find(|a| a.id == id) {
            app.state = state;
            if matches!(state, "running" | "stopped") {
                app.details = json::object();
            }
        } else {
            self.document.record("apps", id)?;
            json::push(
                &mut self.apps,
                AppInfo {
                    id: json::copy(id)?,
                    manifest: Value::Null,
                    state,
                    details: json::object(),
                },
                MAX_RECORDS,
            )?;
        }
        if state != "running" {
            self.clear_app_media(id);
        }
        self.publish(event);
        Ok(())
    }

    /// Fout, herstartteller en retrytijd zijn vluchtige supervisorinformatie.
    pub fn app_runtime(&self, id: &str) -> Option<&Value> {
        self.apps.iter().find(|a| a.id == id).map(|a| &a.details)
    }
    /// Publiceert gewijzigde details zonder het huisdocument te herschrijven.
    pub fn set_app_runtime(&mut self, id: &str, details: Value) -> Result {
        if self
            .app_runtime(id)
            .is_some_and(|v| json::equal(v, &details))
        {
            return Ok(());
        }
        let event = self.event("apps", "app.update", id)?;
        let app = self
            .apps
            .iter_mut()
            .find(|a| a.id == id)
            .ok_or(Error::Missing("app runtime missing"))?;
        app.details = details;
        self.publish(event);
        Ok(())
    }
    /// Cursor voor SSE en app-snapshotabonnees.
    pub fn sequence(&self) -> u64 {
        self.sequence
    }

    /// Verlies wordt expliciet: een achtergelopen abonnee moet een snapshot ophalen.
    pub fn events_after(&self, cursor: u64) -> Result<impl Iterator<Item = &Event>> {
        if cursor > self.sequence
            || self
                .events
                .first()
                .is_some_and(|e| cursor < e.sequence.saturating_sub(1))
        {
            return Err(Error::Changed);
        }
        Ok(self.events.iter().filter(move |e| e.sequence > cursor))
    }

    fn event(&self, manager: &'static str, kind: &'static str, id: &str) -> Result<Event> {
        Ok(Event {
            sequence: self.sequence.checked_add(1).ok_or(Error::Full)?,
            manager,
            kind,
            id: json::copy(id)?,
        })
    }

    fn publish(&mut self, event: Event) {
        self.sequence = event.sequence;
        if self.events.len() == EVENT_CAPACITY {
            self.events.remove(0);
        }
        // INVARIANT: open reserveert de hele ring, en remove houdt een vrije plek.
        self.events.push(event);
    }

    fn commit(&mut self, candidate: Document, event: Event) -> Result {
        let encoded = candidate.encode()?;
        self.storage.save(encoded.as_bytes())?;
        self.document = candidate;
        self.publish(event);
        Ok(())
    }

    /// Vervangt een gevalideerd archiefdocument en vergeet alle oude runtime-identiteiten.
    /// De adapter moet vooraf processen en lopende callbacks hebben gestopt.
    pub fn restore(&mut self, bytes: &[u8]) -> Result {
        let mut candidate = Document::decode(bytes)?;
        scenes::reconcile(&mut candidate)?;
        let event = self.event("store", "store.reload", "")?;
        let encoded = candidate.encode()?;
        self.storage.save(encoded.as_bytes())?;
        self.document = candidate;
        self.live.clear();
        self.apps.clear();
        self.caches.clear();
        self.triggers.clear();
        self.scene_trips.clear();
        self.media.clear();
        self.images.clear();
        self.statistics = Default::default();
        self.statistics_fault = None;
        self.statistics_reported = false;
        self.events.clear();
        self.publish(event);
        Ok(())
    }

    /// Een configuratierecord aanmaken of vervangen met optionele revisiecontrole.
    pub fn put(
        &mut self,
        collection: &str,
        mut record: Value,
        create: bool,
        revision: Option<u64>,
        now: &str,
    ) -> Result {
        let (manager, created, updated, _) = collection_events(collection)?;
        let id = json::copy(json::text(&record, "id"))?;
        if id.is_empty() {
            return Err(Error::Invalid("record id is required"));
        }
        let cache = if collection == "devices" {
            self.prepare_cache(&id, &record)?
        } else {
            None
        };
        let previous = self.document.record(collection, &id).ok();
        if create && previous.is_some() {
            return Err(Error::Conflict("record already exists"));
        }
        if !create && previous.is_none() {
            return Err(Error::Missing("record does not exist"));
        }
        if revision.is_some_and(|r| previous.map(|p| json::uint(p, "revision")) != Some(r)) {
            return Err(Error::Changed);
        }
        if matches!(collection, "flows" | "scenes") {
            let next = previous
                .map(|p| json::uint(p, "revision"))
                .unwrap_or(0)
                .checked_add(1)
                .ok_or(Error::Full)?;
            json::set(&mut record, "revision", Value::uint(next))?;
        }
        for key in ["createdAt", "lastRunAt", "lastError"] {
            if let Some(value) = previous.and_then(|p| json::get(p, key)) {
                json::set(&mut record, key, value.try_clone()?)?;
            }
        }
        if create {
            json::set(&mut record, "createdAt", json::string(now)?)?;
        }
        json::set(&mut record, "updatedAt", json::string(now)?)?;
        self.normalize(collection, &mut record, previous)?;
        let event = self.event(manager, if create { created } else { updated }, &id)?;
        let mut candidate = self.document.candidate()?;
        replace_record(&mut candidate, collection, record)?;
        if collection == "scenes" {
            scenes::reconcile(&mut candidate)?;
        }
        self.commit(candidate, event)?;
        if let Some(cache) = cache {
            if let Some(previous) = self.caches.iter_mut().find(|(key, _)| key == &id) {
                previous.1 = cache.1;
            } else {
                self.caches.push(cache);
            }
        }
        Ok(())
    }

    fn prepare_cache(&mut self, id: &str, record: &Value) -> Result<Option<(String, Value)>> {
        let Some(store) = json::get(record, "store").and_then(Value::as_object) else {
            return Ok(None);
        };
        let mut cache = json::Object::new();
        for (key, value) in store.iter().filter(|(key, _)| key.starts_with('~')) {
            cache.push(key, value.try_clone()?)?;
        }
        if cache.is_empty() {
            return Ok(None);
        }
        if !self.caches.iter().any(|(key, _)| key == id) {
            if self.caches.len() == MAX_RECORDS {
                return Err(Error::Full);
            }
            self.caches.try_reserve(1).map_err(|_| Error::Memory)?;
        }
        Ok(Some((json::copy(id)?, Value::Object(cache))))
    }

    fn normalize(&self, collection: &str, record: &mut Value, previous: Option<&Value>) -> Result {
        match collection {
            "flows" => {
                let name = json::copy(json::text(record, "name").trim())?;
                json::set(record, "name", json::string(&name)?)?;
                crate::flow::validate(record)?;
                scenes::validate_references(&self.document, record)
            }
            "deviceGroups" => groups::validate(&self.document, record),
            "scenes" => {
                if let Some(old) = previous
                    && json::text(old, "kind") != json::text(record, "kind")
                {
                    scenes::can_delete(&self.document, old)?;
                }
                scenes::validate(record, previous)
            }
            "devices" => self.normalize_device(record, previous),
            "notifications" => {
                let excerpt = json::copy(json::text(record, "excerpt").trim())?;
                if excerpt.is_empty() || excerpt.chars().count() > 500 {
                    return Err(Error::Invalid(
                        "notification excerpt must contain 1 to 500 characters",
                    ));
                }
                json::set(record, "excerpt", json::string(&excerpt)?)
            }
            "apps" => Ok(()),
            _ => Err(Error::Invalid("unknown collection")),
        }
    }

    fn normalize_device(&self, record: &mut Value, previous: Option<&Value>) -> Result {
        if json::text(record, "appId") != scenes::APP_ID {
            self.document.record("apps", json::text(record, "appId"))?;
        }
        let group = json::text(record, "groupId");
        if !group.is_empty() {
            self.document.record("deviceGroups", group)?;
        }
        if let Some(old) = previous {
            for key in ["appId", "driverId", "data"] {
                if !json::equal(
                    json::get(old, key).unwrap_or(&Value::Null),
                    json::get(record, key).unwrap_or(&Value::Null),
                ) {
                    return Err(Error::Invalid("paired device identity is immutable"));
                }
            }
        }
        for existing in self.document.records("devices") {
            if json::text(existing, "id") != json::text(record, "id")
                && json::text(existing, "appId") == json::text(record, "appId")
                && json::text(existing, "driverId") == json::text(record, "driverId")
                && json::equal(
                    json::get(existing, "data").unwrap_or(&Value::Null),
                    json::get(record, "data").unwrap_or(&Value::Null),
                )
            {
                return Err(Error::Conflict("device is already paired"));
            }
        }
        for key in ["data", "settings", "store"] {
            if json::get(record, key).is_none_or(Value::is_null) {
                json::set(record, key, json::object())?;
            }
        }
        let mut store = json::get(record, "store")
            .ok_or(Error::Missing("device store"))?
            .try_clone()?;
        if json::text(&store, "__stulp.hardwareName").is_empty() {
            json::set(
                &mut store,
                "__stulp.hardwareName",
                json::string(json::text(record, "name").trim())?,
            )?;
        }
        let mut persistent = json::Object::new();
        for (key, value) in store
            .as_object()
            .ok_or(Error::Invalid("device store must be an object"))?
            .iter()
            .filter(|(key, _)| !key.starts_with('~'))
        {
            persistent.push(key, value.try_clone()?)?;
        }
        json::set(record, "store", Value::Object(persistent))?;
        for key in ["state", "available", "unavailableMessage"] {
            json::remove(record, key)?;
        }
        Ok(())
    }

    /// Verwijdert een record; groepsinhoud wordt naar de ouder verplaatst.
    pub fn delete(&mut self, collection: &str, id: &str) -> Result {
        let previous = self.document.record(collection, id)?;
        if collection == "scenes" {
            scenes::can_delete(&self.document, previous)?;
        }
        if collection == "devices" && json::text(previous, "appId") == scenes::APP_ID {
            return Err(Error::Invalid(
                "scene device must be deleted through its scene",
            ));
        }
        let (manager, _, _, kind) = collection_events(collection)?;
        let event = self.event(manager, kind, id)?;
        let mut candidate = self.document.candidate()?;
        if collection == "deviceGroups" {
            groups::lift_children(&mut candidate, id)?;
        }
        if collection == "apps" {
            uninstall(&mut candidate, id)?;
        }
        remove_record(&mut candidate, collection, id)?;
        if collection == "scenes" {
            scenes::reconcile(&mut candidate)?;
        }
        self.commit(candidate, event)?;
        if collection == "apps" {
            self.apps.retain(|a| a.id != id);
            self.media
                .retain(|(key, _)| self.document.record("devices", key).is_ok());
        }
        if collection == "devices" {
            self.media.retain(|(key, _)| key != id);
        }
        self.live
            .retain(|o| self.document.record("devices", &o.id).is_ok());
        self.caches
            .retain(|(id, _)| self.document.record("devices", id).is_ok());
        Ok(())
    }

    /// Instellingen blijven per app afgeschermd; None verwijdert de sleutel.
    pub fn setting(&mut self, app_id: &str, key: &str, value: Option<Value>) -> Result {
        self.document.record("apps", app_id)?;
        let mut candidate = self.document.candidate()?;
        let mut settings = json::get(candidate.root(), "appSettings")
            .ok_or(Error::Missing("appSettings"))?
            .try_clone()?;
        let mut app = match json::get(&settings, app_id) {
            Some(v) => v.try_clone()?,
            None => json::object(),
        };
        match value {
            Some(value) => json::set(&mut app, key, value)?,
            None => json::remove(&mut app, key)?,
        }
        json::set(&mut settings, app_id, app)?;
        candidate.set("appSettings", settings)?;
        let event = self.event("apps", "app.settings", app_id)?;
        self.commit(candidate, event)
    }

    /// Opaque appstaat komt uitsluitend via het geauthenticeerde appkanaal binnen.
    pub fn app_state(&mut self, app_id: &str, value: Value) -> Result {
        self.document.record("apps", app_id)?;
        let mut candidate = self.document.candidate()?;
        let mut state = json::get(candidate.root(), "appState")
            .ok_or(Error::Missing("appState"))?
            .try_clone()?;
        json::set(&mut state, app_id, value)?;
        candidate.set("appState", state)?;
        let event = self.event("apps", "app.state", app_id)?;
        self.commit(candidate, event)
    }

    /// Slaat systeeminstellingen op; de transportlaag filtert geheime velden.
    pub fn system(&mut self, value: Value) -> Result {
        if value.as_object().is_none() {
            return Err(Error::Invalid("system settings must be an object"));
        }
        let mut candidate = self.document.candidate()?;
        candidate.set("system", value)?;
        let event = self.event("system", "system.update", "system")?;
        self.commit(candidate, event)?;
        if !self.statistics_enabled() {
            self.statistics = Default::default();
            self.statistics_fault = None;
            self.statistics_reported = false;
        }
        Ok(())
    }

    /// Een afgeronde Flow schrijft historie; een oudere start overschrijft geen nieuwere.
    pub fn flow_result(&mut self, id: &str, ran_at: &str, error: &str, now: &str) -> Result {
        let previous = self.document.record("flows", id)?;
        if json::time_is_before(ran_at, json::text(previous, "lastRunAt")) {
            return Ok(());
        }
        let mut record = previous.try_clone()?;
        json::set(&mut record, "lastRunAt", json::string(ran_at)?)?;
        json::set(&mut record, "updatedAt", json::string(now)?)?;
        json::set(&mut record, "lastError", json::string(error)?)?;
        let revision = json::uint(&record, "revision")
            .checked_add(1)
            .ok_or(Error::Full)?;
        json::set(&mut record, "revision", Value::uint(revision))?;
        let mut candidate = self.document.candidate()?;
        replace_record(&mut candidate, "flows", record)?;
        let event = self.event("flow", "flow.run", id)?;
        self.commit(candidate, event)
    }

    /// De scene-eigenaar controleert de melding opnieuw zodra een lopende activatie klaar is.
    pub fn take_scene_trip(&mut self) -> Option<Value> {
        self.scene_trips.pop()
    }
    /// Een observatie schrijft nooit flash en accepteert alleen de eigenaar van het device.
    pub fn observe(
        &mut self,
        app_id: &str,
        id: &str,
        state: Value,
        available: bool,
        message: &str,
    ) -> Result {
        let record = self.document.record("devices", id)?;
        if json::text(record, "appId") != app_id {
            return Err(Error::Invalid("device belongs to another app"));
        }
        if state.as_object().is_none() {
            return Err(Error::Invalid("device state must be an object"));
        }
        let trips = if let Some(previous) = self.live.iter().find(|o| o.id == id) {
            scenes::trips(&self.document, id, &previous.state, &state)?
        } else {
            Vec::new()
        };
        if trips.len() > 256usize.saturating_sub(self.scene_trips.len()) {
            return Err(Error::Full);
        }
        self.scene_trips
            .try_reserve(trips.len())
            .map_err(|_| Error::Memory)?;
        let changes = if let Some(previous) = self.live.iter().find(|o| o.id == id) {
            triggers::changes(record, &previous.state, &state)?
        } else {
            Vec::new()
        };
        if changes.len() > MAX_TRIGGERS.saturating_sub(self.triggers.len()) {
            return Err(Error::Full);
        }
        self.triggers
            .try_reserve(changes.len())
            .map_err(|_| Error::Memory)?;
        let sequence = self.sequence.checked_add(1).ok_or(Error::Full)?;
        let previous = self.live.iter().find(|o| o.id == id);
        let mut revisions = json::Object::new();
        if let Some(old) = previous
            && let Some(fields) = old.revisions.as_object()
        {
            for (cap, revision) in fields.iter() {
                let before = json::get(&old.state, cap).unwrap_or(&Value::Null);
                let after = json::get(&state, cap).unwrap_or(&Value::Null);
                revisions.push(
                    cap,
                    if json::equal(before, after) {
                        revision.try_clone()?
                    } else {
                        Value::uint(sequence)
                    },
                )?;
            }
        }
        if let Some(fields) = state.as_object() {
            for (cap, _) in fields.iter() {
                if previous.is_none_or(|old| json::get(&old.revisions, cap).is_none()) {
                    revisions.push(cap, Value::uint(sequence))?;
                }
            }
        }
        let name = json::copy(json::text(record, "name"))?;
        let next = Observation {
            id: json::copy(id)?,
            state,
            revisions: Value::Object(revisions),
            available,
            message: json::copy(message)?,
        };
        let event = self.event("devices", "device.update", id)?;
        if let Some(old) = self.live.iter_mut().find(|o| o.id == id) {
            *old = next;
        } else {
            json::push(&mut self.live, next, MAX_RECORDS)?;
        }
        if self.statistics_enabled()
            && let Some(o) = self.live.iter().find(|o| o.id == id)
            && let Err(e) = self.statistics.observe(id, &name, &o.state, self.clock_ms)
            && !self.statistics_reported
        {
            self.statistics_fault = Some(e);
            self.statistics_reported = true;
        }
        self.triggers.extend(changes);
        self.scene_trips.extend(trips);
        self.publish(event);
        Ok(())
    }

    /// Wijzigingscursor per capability, ook als tussen twee polls heen en weer is geschakeld.
    pub fn capability_revision(&self, id: &str, cap: &str) -> u64 {
        if let Some(o) = self.live.iter().find(|o| o.id == id) {
            return json::uint(&o.revisions, cap);
        }
        id.strip_prefix("scene:")
            .and_then(|id| self.document.record("scenes", id).ok())
            .map(|s| json::uint(s, "revision"))
            .unwrap_or(0)
    }

    /// Bouwt een device-snapshot zonder de duurzame configuratie te vervuilen.
    pub fn device(&self, id: &str) -> Result<Value> {
        let mut out = self.document.record("devices", id)?.try_clone()?;
        if let Some((_, cache)) = self.caches.iter().find(|(key, _)| key == id) {
            let mut store = json::get(&out, "store")
                .unwrap_or(&json::object())
                .try_clone()?;
            if let Some(cache) = cache.as_object() {
                for (key, value) in cache.iter() {
                    json::set(&mut store, key, value.try_clone()?)?;
                }
            }
            json::set(&mut out, "store", store)?;
        }
        let observation = self.live.iter().find(|o| o.id == id);
        let scene = id
            .strip_prefix("scene:")
            .filter(|_| json::text(&out, "appId") == scenes::APP_ID)
            .and_then(|id| self.document.record("scenes", id).ok());
        let state = match scene {
            Some(scene) if json::text(scene, "kind") != "button" => {
                json::fields(&[("onoff", Value::Bool(json::boolean(scene, "active")))])?
            }
            _ => match observation {
                Some(o) => o.state.try_clone()?,
                None => json::object(),
            },
        };
        json::set(&mut out, "state", state)?;
        json::set(
            &mut out,
            "available",
            Value::Bool(scene.is_some() || observation.is_some_and(|o| o.available)),
        )?;
        json::set(
            &mut out,
            "unavailableMessage",
            json::string(observation.map(|o| o.message.as_str()).unwrap_or(""))?,
        )?;
        Ok(out)
    }
}

fn collection_events(
    collection: &str,
) -> Result<(&'static str, &'static str, &'static str, &'static str)> {
    Ok(match collection {
        "apps" => ("apps", "app.create", "app.update", "app.delete"),
        "devices" => ("devices", "device.create", "device.update", "device.delete"),
        "deviceGroups" => ("devices", "group.create", "group.update", "group.delete"),
        "flows" => ("flow", "flow.create", "flow.update", "flow.delete"),
        "scenes" => ("scene", "scene.create", "scene.update", "scene.delete"),
        "notifications" => (
            "notifications",
            "notification.create",
            "notification.update",
            "notification.delete",
        ),
        _ => return Err(Error::Invalid("unknown collection")),
    })
}

fn replace_record(doc: &mut Document, collection: &str, record: Value) -> Result {
    let id = json::text(&record, "id");
    let mut records = Vec::new();
    for previous in doc
        .records(collection)
        .iter()
        .filter(|r| json::text(r, "id") != id)
    {
        json::push(&mut records, previous.try_clone()?, MAX_RECORDS)?;
    }
    json::push(&mut records, record, MAX_RECORDS)?;
    if collection == "notifications" {
        records.sort_unstable_by(|a, b| json::text(b, "createdAt").cmp(json::text(a, "createdAt")));
        records.truncate(crate::document::MAX_NOTIFICATIONS);
    }
    doc.replace(collection, records)
}

fn remove_record(doc: &mut Document, collection: &str, id: &str) -> Result {
    let mut records = Vec::new();
    for record in doc
        .records(collection)
        .iter()
        .filter(|r| json::text(r, "id") != id)
    {
        json::push(&mut records, record.try_clone()?, MAX_RECORDS)?;
    }
    doc.replace(collection, records)
}

fn uninstall(doc: &mut Document, app_id: &str) -> Result {
    let mut flows = Vec::new();
    for flow in doc.records("flows") {
        let mut flow = flow.try_clone()?;
        if references_app(doc, &flow, app_id) {
            json::set(&mut flow, "enabled", Value::Bool(false))?;
            json::set(&mut flow, "lastError", json::string("app was uninstalled")?)?;
            let revision = json::uint(&flow, "revision")
                .checked_add(1)
                .ok_or(Error::Full)?;
            json::set(&mut flow, "revision", Value::uint(revision))?;
        }
        json::push(&mut flows, flow, MAX_RECORDS)?;
    }
    doc.replace("flows", flows)?;
    let mut devices = Vec::new();
    for device in doc
        .records("devices")
        .iter()
        .filter(|d| json::text(d, "appId") != app_id)
    {
        json::push(&mut devices, device.try_clone()?, MAX_RECORDS)?;
    }
    doc.replace("devices", devices)?;
    for key in ["appSettings", "appState"] {
        let mut value = json::get(doc.root(), key)
            .ok_or(Error::Missing("app configuration"))?
            .try_clone()?;
        json::remove(&mut value, app_id)?;
        doc.set(key, value)?;
    }
    Ok(())
}

fn references_app(doc: &Document, value: &Value, app_id: &str) -> bool {
    match value {
        Value::Object(o) => {
            if json::text(value, "appId") == app_id {
                return true;
            }
            if let Some(id) = o.get("$device").and_then(Value::as_str)
                && doc
                    .record("devices", id)
                    .is_ok_and(|d| json::text(d, "appId") == app_id)
            {
                return true;
            }
            o.iter().any(|(_, v)| references_app(doc, v, app_id))
        }
        Value::Array(a) => a.iter().any(|v| references_app(doc, v, app_id)),
        _ => false,
    }
}

/// Dezelfde scene-tolerantie voor planning, herstel en het volgen van apparaatrapporten.
pub fn scene_values_equal(a: &Value, b: &Value) -> bool {
    scenes::same(a, b)
}
