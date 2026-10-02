//! Native response routing for the shared catalog owner.
pub(crate) type Catalog =
    stulp_controller::catalog::Catalog<std::sync::mpsc::SyncSender<stulp_web::Response>>;
