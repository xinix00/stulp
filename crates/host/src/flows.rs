//! Native response routing for the shared flows owner.
pub(crate) type Flows =
    stulp_controller::flows::Flows<std::sync::mpsc::SyncSender<stulp_web::Response>>;
