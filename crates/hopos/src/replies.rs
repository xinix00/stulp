//! Generation-checked response handles on the app's sole executor core.
//! The controller writes replies; each HTTP/MCP receiver consumes its own slot.
//! A dropped receiver revokes the handle before the slot can be reused.
use core::{cell::RefCell, marker::PhantomData};
use hop_sync::{Local, Signal};
use stulp_controller::{Inbox, Reply};
use stulp_core::{Error, Result};
use stulp_web::Response;
const CAP: usize = 128;
struct Slot {
    generation: u64,
    used: bool,
    response: Option<Response>,
}
static SLOTS: Local<RefCell<[Slot; CAP]>> = Local::cell(
    [const {
        Slot {
            generation: 0,
            used: false,
            response: None,
        }
    }; CAP],
);
static READY: [Signal; CAP] = [const { Signal::new() }; CAP];

/// Copyable route, deliberately neither Send nor Sync across executor cores.
#[derive(Clone)]
pub struct Sender {
    index: usize,
    generation: u64,
    local: PhantomData<*const ()>,
}
/// Sole owner of one response lease; Drop prevents late replies reaching another request.
pub struct Receiver(Sender);
impl Sender {
    pub(crate) fn closed(&self) -> bool {
        SLOTS.try_borrow().map_or(true, |slots| {
            let slot = &slots[self.index];
            !slot.used || slot.generation != self.generation
        })
    }
}
impl Reply for Sender {
    type Inbox = Receiver;
    fn channel() -> Result<(Self, Receiver)> {
        let mut slots = SLOTS
            .try_borrow_mut()
            .map_err(|_| Error::Invalid("response table busy"))?;
        let (index, slot) = slots
            .iter_mut()
            .enumerate()
            .find(|(_, s)| !s.used)
            .ok_or(Error::Full)?;
        slot.generation = slot.generation.checked_add(1).ok_or(Error::Full)?;
        slot.used = true;
        slot.response = None;
        READY[index].take();
        let sender = Self {
            index,
            generation: slot.generation,
            local: PhantomData,
        };
        Ok((sender.clone(), Receiver(sender)))
    }
    fn send(&self, response: Response) -> Result {
        {
            let mut slots = SLOTS
                .try_borrow_mut()
                .map_err(|_| Error::Invalid("response table busy"))?;
            let slot = &mut slots[self.index];
            if !slot.used || slot.generation != self.generation {
                return Err(Error::Missing("response route closed"));
            }
            if slot.response.is_some() {
                return Err(Error::Full);
            }
            slot.response = Some(response);
        }
        READY[self.index].set();
        Ok(())
    }
}
impl Inbox for Receiver {
    fn receive(&self) -> Result<Option<Response>> {
        let mut slots = SLOTS
            .try_borrow_mut()
            .map_err(|_| Error::Invalid("response table busy"))?;
        let slot = &mut slots[self.0.index];
        if !slot.used || slot.generation != self.0.generation {
            return Err(Error::Missing("response route closed"));
        }
        Ok(slot.response.take())
    }
}
impl Receiver {
    /// Wait without spinning; signals cannot leak a response across generations.
    pub async fn wait(&self) -> Result<Response> {
        loop {
            if let Some(response) = self.receive()? {
                return Ok(response);
            }
            READY[self.0.index].wait().await;
        }
    }
}
impl Drop for Receiver {
    fn drop(&mut self) {
        if let Ok(mut slots) = SLOTS.try_borrow_mut() {
            let slot = &mut slots[self.0.index];
            if slot.generation == self.0.generation {
                slot.used = false;
                slot.response = None;
            }
        }
    }
}
