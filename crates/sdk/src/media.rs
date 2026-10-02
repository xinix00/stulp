//! Binaire media blijft buiten JSON-RPC; de transportadapter bezit de lokale HTTP-uitvoer.
use alloc::{string::String, vec::Vec};
/// Opdracht aan de ene media-eigenaar.
pub enum MediaCommand {
    /// Maak één private bron beschikbaar nadat de codec bekend is.
    Register {
        /// Monotoon stream-id, niet hergebruiken binnen een attach.
        id: u64,
        /// Willekeurig URL-token, uitsluitend letters/cijfers/koppelteken/underscore.
        token: String,
        /// Volledig MIME-type inclusief codec.
        mime: String,
        /// Initialisatiesegment.
        header: Vec<u8>,
    },
    /// Verplaatst één volledig fragment naar de HTTP-eigenaar.
    Frame {
        /// Geregistreerde bron.
        id: u64,
        /// Nieuwe kijkers mogen hier instappen.
        keyframe: bool,
        /// MP4-fragment.
        bytes: Vec<u8>,
    },
    /// Sluit bron en alle kijkers.
    Close {
        /// Geregistreerde bron.
        id: u64,
    },
}
/// Terugmelding uit de media-eigenaar.
pub enum MediaEvent {
    /// Dertig seconden zonder kijkers: stop ook de cameraverbinding.
    Idle(u64),
    /// Bron afgewezen of uitvoer mislukt.
    Failed(u64, super::Error),
}
