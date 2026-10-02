//! Native response routing for the shared scenes owner.
pub(crate) type Scenes =
    stulp_controller::scenes::Scenes<std::sync::mpsc::SyncSender<stulp_web::Response>>;
