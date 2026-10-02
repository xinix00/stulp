//! Eén lokale of TCP-byteverbinding; protocol en levensloop blijven bij de eigenaar.
use std::{
    io::{self, Read, Write},
    net::{Shutdown, TcpStream},
    os::unix::net::UnixStream,
};
/// Geordende appbytes zonder gedeeld eigendom.
pub enum Socket {
    /// Expliciet geconfigureerde externe verbinding.
    Tcp(TcpStream),
    /// Privé Unix-socket van een lokaal gestart proces.
    Unix(UnixStream),
}
impl Socket {
    /// Maakt beide adapters geschikt voor dezelfde begrensde pomp.
    pub fn nonblocking(&self) -> io::Result<()> {
        match self {
            Self::Tcp(s) => {
                s.set_nodelay(true)?;
                s.set_nonblocking(true)
            }
            Self::Unix(s) => s.set_nonblocking(true),
        }
    }
    /// Herroept de verbinding; de volgende pomp levert alle openstaande callbacks terug.
    pub fn shutdown(&self) -> io::Result<()> {
        match self {
            Self::Tcp(s) => s.shutdown(Shutdown::Both),
            Self::Unix(s) => s.shutdown(Shutdown::Both),
        }
    }
}
impl Read for Socket {
    fn read(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
        match self {
            Self::Tcp(s) => s.read(bytes),
            Self::Unix(s) => s.read(bytes),
        }
    }
}
impl Write for Socket {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        match self {
            Self::Tcp(s) => s.write(bytes),
            Self::Unix(s) => s.write(bytes),
        }
    }
    fn flush(&mut self) -> io::Result<()> {
        match self {
            Self::Tcp(s) => s.flush(),
            Self::Unix(s) => s.flush(),
        }
    }
}

/// Dupliceert het door de ouder geërfde controlekanaal vóór andere adapters descriptors openen.
/// De kopie is exclusief eigendom, CLOEXEC en aantoonbaar een verbonden Unix-socket.
pub fn inherited_control() -> io::Result<UnixStream> {
    let stream = duplicate_control(3)?;
    // SAFETY: fcntl valideert descriptor 3; deze bootfunctie draait vóór de plugin I/O-workers opent.
    let flags = unsafe { libc::fcntl(3, libc::F_GETFD) };
    // SAFETY: De geërfde descriptor blijft open. CLOEXEC voorkomt dat dns-sd of een volgend kind het kanaal erft.
    if flags < 0 || unsafe { libc::fcntl(3, libc::F_SETFD, flags | libc::FD_CLOEXEC) } < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(stream)
}
fn duplicate_control(fd: std::os::fd::RawFd) -> io::Result<UnixStream> {
    use std::os::fd::{FromRawFd, OwnedFd};
    // SAFETY: fcntl valideert fd in de kernel en levert bij succes een nieuwe exclusieve descriptor.
    let copied = unsafe { libc::fcntl(fd, libc::F_DUPFD_CLOEXEC, 4) };
    if copied < 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: copied is zojuist aangemaakt, nog niet gewrapped en wordt precies eenmaal overgedragen.
    let owned = unsafe { OwnedFd::from_raw_fd(copied) };
    let stream = UnixStream::from(owned);
    stream.peer_addr()?;
    if !crate::peer::same_user(&stream)? {
        return Err(io::ErrorKind::PermissionDenied.into());
    }
    Ok(stream)
}
#[cfg(all(test, not(miri)))]
mod tests {
    use super::*;
    use std::os::fd::AsRawFd;
    #[test]
    fn inherited_copy_does_not_steal_or_close_the_original() -> io::Result<()> {
        let (original, mut peer) = UnixStream::pair()?;
        let mut copy = duplicate_control(original.as_raw_fd())?;
        copy.write_all(b"one")?;
        let mut bytes = [0; 3];
        peer.read_exact(&mut bytes)?;
        assert_eq!(&bytes, b"one");
        drop(copy);
        (&original).write_all(b"two")?;
        peer.read_exact(&mut bytes)?;
        assert_eq!(&bytes, b"two");
        assert!(duplicate_control(-1).is_err());
        let file = std::fs::File::open("/dev/null")?;
        assert!(duplicate_control(file.as_raw_fd()).is_err());
        Ok(())
    }
}

/// Zet uitsluitend O_NONBLOCK op een geleende pipe of socket; de eigenaar houdt de descriptor.
pub fn nonblocking(handle: &impl std::os::fd::AsFd) -> io::Result<()> {
    use std::os::fd::AsRawFd;
    let fd = handle.as_fd().as_raw_fd();
    // SAFETY: De AsFd-lening houdt fd geldig gedurende beide fcntl-aanroepen; F_GETFL schrijft geen geheugen.
    let flags = unsafe { libc::fcntl(fd, libc::F_GETFL) };
    if flags < 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: F_SETFL ontvangt alleen vlaggen, en fd blijft door dezelfde lening geldig.
    if unsafe { libc::fcntl(fd, libc::F_SETFL, flags | libc::O_NONBLOCK) } < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}
