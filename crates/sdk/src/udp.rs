//! Langlevende datagramsockets: de plugin bezit het protocol, de adapter de sockets.
use crate::Error;
use alloc::{string::String, vec::Vec};
/// Maximaal acht sockets; IDs zijn niet-nul en stijgen gedurende één attach.
pub enum UdpCommand {
    /// Bind op een letterlijk IPv4/IPv6-adres met poort; IPv6-scopes zijn numeriek.
    Bind {
        /// Uniek eigendomsnummer.
        id: u64,
        /// Bijvoorbeeld `[::]:0` of `0.0.0.0:0`.
        address: String,
    },
    /// Verzend één datagram van maximaal 8192 bytes; de adapter herhaalt het nooit.
    Send {
        /// Eerder geopende socket.
        id: u64,
        /// Letterlijk bestemmingsadres met poort en eventuele scope.
        address: String,
        /// Volledig datagram, ook een leeg datagram is geldig.
        bytes: Vec<u8>,
    },
    /// Word lid van een IPv4-multicastgroep op een expliciet interfaceadres.
    JoinV4 {
        /// Eerder geopende socket.
        id: u64,
        /// Multicastadres.
        group: [u8; 4],
        /// Interfaceadres, of alle nul voor de standaardroute.
        interface: [u8; 4],
    },
    /// Word lid van een IPv6-multicastgroep op een expliciete interface-index.
    JoinV6 {
        /// Eerder geopende socket.
        id: u64,
        /// Multicastadres.
        group: [u8; 16],
        /// Interface-index voor link-local multicast.
        interface: u32,
    },
    /// Sluit de socket. Een later Bind krijgt altijd een nieuw ID.
    Close {
        /// Eerder geopende socket.
        id: u64,
    },
}
/// Geen streamframing: ieder event behoudt één volledig datagram en zijn bron.
pub enum UdpEvent {
    /// De socket is gebonden; het tweede veld is het canonieke lokale adres.
    Bound(u64, String),
    /// Volledig datagram, bronadres en eigendomsnummer.
    Data {
        /// Ontvangende socket.
        id: u64,
        /// Canoniek bronadres, inclusief de numerieke IPv6-scope.
        address: String,
        /// Maximaal 8192 bytes; te grote datagrams worden afgewezen, nooit afgekapt.
        bytes: Vec<u8>,
    },
    /// Een opdracht of ontvangst faalde; een bestaande socket blijft bruikbaar.
    Error(u64, Error),
    /// Expliciet gesloten socket.
    Closed(u64),
}
