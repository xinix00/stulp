//! Native response routing for the shared pairing owner.
pub(crate) type Pairing =
    stulp_controller::pairing::Pairing<std::sync::mpsc::SyncSender<stulp_web::Response>>;
pub(crate) use stulp_controller::pairing::candidate;
