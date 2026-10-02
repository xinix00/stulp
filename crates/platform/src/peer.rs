//! De kernel bevestigt het UID van een lokale peer; de app levert geen eigen identiteit aan.
use std::{io, os::unix::net::UnixStream};
/// Alleen dezelfde effectieve gebruiker mag zonder TCP-token lokaal verbinden.
pub fn same_user(stream: &UnixStream) -> io::Result<bool> {
    // SAFETY: geteuid neemt geen pointers aan en leest uitsluitend de procesidentiteit.
    Ok(platform::uid(stream)? == unsafe { libc::geteuid() })
}
#[cfg(target_os = "macos")]
mod platform {
    use super::*;
    use std::os::fd::AsRawFd;
    pub(super) fn uid(stream: &UnixStream) -> io::Result<libc::uid_t> {
        let mut uid = 0;
        let mut gid = 0;
        // SAFETY: De geleende socket blijft open en beide uitvoerpointers zijn geldig en exclusief.
        if unsafe { libc::getpeereid(stream.as_raw_fd(), &raw mut uid, &raw mut gid) } == 0 {
            Ok(uid)
        } else {
            Err(io::Error::last_os_error())
        }
    }
}
#[cfg(target_os = "linux")]
mod platform {
    use super::*;
    use std::os::fd::AsRawFd;
    pub(super) fn uid(stream: &UnixStream) -> io::Result<libc::uid_t> {
        let mut credentials = libc::ucred {
            pid: 0,
            uid: 0,
            gid: 0,
        };
        let mut length = size_of::<libc::ucred>() as libc::socklen_t;
        // SAFETY: De kernel schrijft hoogstens length bytes in deze levende, uitgelijnde ucred.
        if unsafe {
            libc::getsockopt(
                stream.as_raw_fd(),
                libc::SOL_SOCKET,
                libc::SO_PEERCRED,
                (&raw mut credentials).cast(),
                &raw mut length,
            )
        } != 0
        {
            return Err(io::Error::last_os_error());
        }
        if length as usize != size_of::<libc::ucred>() {
            return Err(io::ErrorKind::InvalidData.into());
        }
        Ok(credentials.uid)
    }
}
#[cfg(not(any(target_os = "macos", target_os = "linux")))]
mod platform {
    use super::*;
    pub(super) fn uid(_: &UnixStream) -> io::Result<libc::uid_t> {
        Err(io::ErrorKind::Unsupported.into())
    }
}
#[cfg(all(test, not(miri)))]
mod tests {
    use super::*;
    #[test]
    fn local_socket_pair_has_kernel_verified_owner() -> io::Result<()> {
        let (a, b) = UnixStream::pair()?;
        assert!(same_user(&a)?);
        assert!(same_user(&b)?);
        Ok(())
    }
}
