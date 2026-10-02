//! Native response routing for the shared mcp owner.
pub(crate) type Mcp = stulp_controller::mcp::Mcp<std::sync::mpsc::SyncSender<stulp_web::Response>>;
pub(crate) type Services<'a> =
    stulp_controller::mcp::Services<'a, std::sync::mpsc::SyncSender<stulp_web::Response>>;
