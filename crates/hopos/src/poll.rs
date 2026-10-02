//! One nonblocking I/O turn; the controller's bounded timer round retries Pending.
use core::{
    future::Future,
    pin::pin,
    task::{Context, Poll, Waker},
};
pub(crate) fn once<F: Future>(future: F) -> Option<F::Output> {
    match pin!(future).poll(&mut Context::from_waker(Waker::noop())) {
        Poll::Ready(value) => Some(value),
        Poll::Pending => None,
    }
}
