//! Kleine host-ABI-grens voor interfacekeuze; bezit geen protocol, plugin of huisstaat.
pub mod peer;
pub mod signals;
pub mod socket;
use std::{
    io,
    net::{IpAddr, UdpSocket},
};
/// Een adres op een netwerkinterface, met flags uit dezelfde OS-snapshot.
pub struct Interface {
    /// Numerieke IPv6-scope en stabiele interfacekeuze tijdens deze zoekronde.
    pub index: u32,
    /// Eén IPv4- of IPv6-adres.
    pub address: IpAddr,
    /// Alleen actieve multicast-LANs; loopback en point-to-point VPNs vallen af.
    pub lan: bool,
}
/// Leest hoogstens 256 adressen; het C-snapshot wordt ook bij fouten vrijgegeven.
pub fn interfaces() -> io::Result<Vec<Interface>> {
    platform::interfaces()
}
/// Selecteert de uitgaande multicast-interface, ook wanneer de socket aan wildcard bindt.
pub fn multicast_interface(socket: &UdpSocket, address: IpAddr, index: u32) -> io::Result<()> {
    platform::multicast_interface(socket, address, index)
}
/// IPv6 multicast hop-limit is niet beschikbaar in std; deze grens vraagt alleen een socketoptie.
pub fn multicast_hops_v6(socket: &UdpSocket, hops: u8) -> io::Result<()> {
    platform::multicast_hops_v6(socket, hops)
}

#[cfg(any(target_os = "macos", target_os = "linux"))]
mod platform {
    use super::*;
    use std::{
        net::{Ipv4Addr, Ipv6Addr},
        os::fd::AsRawFd,
        ptr,
    };
    struct Snapshot(*mut libc::ifaddrs);
    impl Drop for Snapshot {
        fn drop(&mut self) {
            // SAFETY: Alleen een succesvolle getifaddrs geeft dit exclusieve snapshot; Drop loopt eenmaal.
            unsafe { libc::freeifaddrs(self.0) };
        }
    }
    fn lan(flags: libc::c_uint) -> bool {
        flags & (libc::IFF_UP as u32 | libc::IFF_MULTICAST as u32)
            == (libc::IFF_UP as u32 | libc::IFF_MULTICAST as u32)
            && flags & (libc::IFF_LOOPBACK as u32 | libc::IFF_POINTOPOINT as u32) == 0
    }
    /// Leest alleen de bij de adresfamilie horende layout.
    ///
    /// # Safety
    /// `raw` wijst naar een levende, uitgelijnde sockaddr-header en voor AF_INET/AF_INET6
    /// naar de volledige overeenkomstige sockaddr_in/sockaddr_in6. Een nullpointer is toegestaan.
    unsafe fn address(raw: *const libc::sockaddr) -> Option<IpAddr> {
        if raw.is_null() {
            return None;
        }
        // SAFETY: De caller garandeert de geldige header zolang deze functie loopt.
        let family = unsafe { (*raw).sa_family };
        match libc::c_int::from(family) {
            libc::AF_INET => {
                // SAFETY: Het contract garandeert voor deze familie een volledige uitgelijnde sockaddr_in.
                let value = unsafe { &*raw.cast::<libc::sockaddr_in>() };
                Some(IpAddr::V4(Ipv4Addr::from(
                    value.sin_addr.s_addr.to_ne_bytes(),
                )))
            }
            libc::AF_INET6 => {
                // SAFETY: Het contract garandeert voor deze familie een volledige uitgelijnde sockaddr_in6.
                let value = unsafe { &*raw.cast::<libc::sockaddr_in6>() };
                Some(IpAddr::V6(Ipv6Addr::from(value.sin6_addr.s6_addr)))
            }
            _ => None,
        }
    }
    pub(super) fn interfaces() -> io::Result<Vec<Interface>> {
        let mut head = ptr::null_mut();
        // SAFETY: head is een schrijfbare pointerlocal; de C-functie initialiseert hem bij succes.
        if unsafe { libc::getifaddrs(&raw mut head) } != 0 {
            return Err(io::Error::last_os_error());
        }
        let snapshot = Snapshot(head);
        let mut current = snapshot.0;
        let mut out = Vec::new();
        let mut count = 0;
        while !current.is_null() {
            count += 1;
            if count > 4096 {
                return Err(io::Error::other(
                    "interface snapshot exceeds traversal bound",
                ));
            }
            // SAFETY: getifaddrs levert een eindige gekoppelde lijst die geldig blijft tot snapshot gedropt wordt.
            let item = unsafe { &*current };
            current = item.ifa_next;
            if item.ifa_addr.is_null() || item.ifa_name.is_null() {
                continue;
            }
            // SAFETY: getifaddrs garandeert voor elk niet-null adres het bij sa_family passende sockaddr-type.
            let Some(address) = (unsafe { address(item.ifa_addr) }) else {
                continue;
            };
            // SAFETY: ifa_name is een nulbeëindigde C-string uit de nog levende getifaddrs-snapshot.
            let index = unsafe { libc::if_nametoindex(item.ifa_name) };
            if index == 0
                || out
                    .iter()
                    .any(|v: &Interface| v.index == index && v.address == address)
            {
                continue;
            }
            if out.len() >= 256 {
                return Err(io::Error::other("interface address count exceeds 256"));
            }
            out.try_reserve(1)
                .map_err(|_| io::Error::from(io::ErrorKind::OutOfMemory))?;
            out.push(Interface {
                index,
                address,
                lan: lan(item.ifa_flags),
            });
        }
        Ok(out)
    }
    pub(super) fn multicast_interface(
        socket: &UdpSocket,
        address: IpAddr,
        index: u32,
    ) -> io::Result<()> {
        let status = match address {
            IpAddr::V4(address) => {
                let address = libc::in_addr {
                    s_addr: u32::from_ne_bytes(address.octets()),
                };
                // SAFETY: De geleende socket houdt de descriptor open; optie, pointertype en lengte zijn IP_MULTICAST_IF's ABI.
                unsafe {
                    libc::setsockopt(
                        socket.as_raw_fd(),
                        libc::IPPROTO_IP,
                        libc::IP_MULTICAST_IF,
                        ptr::from_ref(&address).cast(),
                        size_of::<libc::in_addr>() as libc::socklen_t,
                    )
                }
            }
            IpAddr::V6(_) => {
                let index: libc::c_uint = index;
                // SAFETY: De geleende socket houdt de descriptor open; de kernel kopieert deze c_uint binnen de aanroep.
                unsafe {
                    libc::setsockopt(
                        socket.as_raw_fd(),
                        libc::IPPROTO_IPV6,
                        libc::IPV6_MULTICAST_IF,
                        ptr::from_ref(&index).cast(),
                        size_of::<libc::c_uint>() as libc::socklen_t,
                    )
                }
            }
        };
        if status == 0 {
            Ok(())
        } else {
            Err(io::Error::last_os_error())
        }
    }
    pub(super) fn multicast_hops_v6(socket: &UdpSocket, hops: u8) -> io::Result<()> {
        let hops = libc::c_int::from(hops);
        // SAFETY: Geldige geleende descriptor en de c_int-optie blijven gedurende setsockopt leven; de kernel kopieert ze.
        let status = unsafe {
            libc::setsockopt(
                socket.as_raw_fd(),
                libc::IPPROTO_IPV6,
                libc::IPV6_MULTICAST_HOPS,
                ptr::from_ref(&hops).cast(),
                size_of::<libc::c_int>() as libc::socklen_t,
            )
        };
        if status == 0 {
            Ok(())
        } else {
            Err(io::Error::last_os_error())
        }
    }
    #[cfg(test)]
    mod tests {
        use super::*;
        #[test]
        fn vpn_loopback_and_down_interfaces_are_not_lans() {
            let flags = (libc::IFF_UP | libc::IFF_MULTICAST | libc::IFF_BROADCAST) as u32;
            assert!(lan(flags));
            assert!(!lan(flags | libc::IFF_POINTOPOINT as u32));
            assert!(!lan(flags | libc::IFF_LOOPBACK as u32));
            assert!(!lan(flags & !(libc::IFF_UP as u32)));
        }
        #[test]
        fn address_layouts_preserve_network_byte_order() {
            // SAFETY: sockaddr_in bestaat uitsluitend uit integers en integer-arrays, waarvoor alle nulbytes geldig zijn.
            let mut v4: libc::sockaddr_in = unsafe { std::mem::zeroed() };
            v4.sin_family = libc::AF_INET as libc::sa_family_t;
            v4.sin_addr.s_addr = u32::from_ne_bytes([192, 0, 2, 51]);
            // SAFETY: De pointer komt van de levende, uitgelijnde v4-local met de bijpassende adresfamilie.
            let decoded = unsafe { address(ptr::from_ref(&v4).cast()) };
            assert_eq!(decoded, Some(IpAddr::V4(Ipv4Addr::new(192, 0, 2, 51))));
            // SAFETY: sockaddr_in6 bestaat uitsluitend uit integers en integer-arrays, waarvoor nul geldig is.
            let mut v6: libc::sockaddr_in6 = unsafe { std::mem::zeroed() };
            v6.sin6_family = libc::AF_INET6 as libc::sa_family_t;
            v6.sin6_addr.s6_addr = Ipv6Addr::new(0xfd11, 0, 0, 0, 0, 0, 0, 51).octets();
            // SAFETY: De pointer komt van de levende, uitgelijnde v6-local met de bijpassende adresfamilie.
            let decoded = unsafe { address(ptr::from_ref(&v6).cast()) };
            assert_eq!(
                decoded,
                Some(IpAddr::V6(Ipv6Addr::new(0xfd11, 0, 0, 0, 0, 0, 0, 51)))
            );
            // SAFETY: Null is expliciet toegestaan en wordt niet gedereferenceerd.
            assert!(unsafe { address(ptr::null()) }.is_none());
        }
        // De OS-ABI zelf wordt native getest; Miri biedt geen getifaddrs- of socketsimulatie.
        #[test]
        #[cfg(not(miri))]
        fn snapshot_and_socket_options_use_real_os_abi_without_sending_packets() -> io::Result<()> {
            let addresses = interfaces()?;
            assert!(addresses.iter().any(|a| a.address.is_loopback()));
            let v4 = UdpSocket::bind("127.0.0.1:0")?;
            multicast_interface(&v4, IpAddr::V4(Ipv4Addr::LOCALHOST), 0)?;
            let v6 = UdpSocket::bind("[::1]:0")?;
            multicast_hops_v6(&v6, 255)?;
            if let Some(local) = addresses
                .iter()
                .find(|a| a.address == IpAddr::V6(Ipv6Addr::LOCALHOST))
            {
                multicast_interface(&v6, local.address, local.index)?;
            }
            Ok(())
        }
    }
}
#[cfg(not(any(target_os = "macos", target_os = "linux")))]
mod platform {
    use super::*;
    pub(super) fn interfaces() -> io::Result<Vec<Interface>> {
        Err(io::Error::from(io::ErrorKind::Unsupported))
    }
    pub(super) fn multicast_interface(_: &UdpSocket, _: IpAddr, _: u32) -> io::Result<()> {
        Err(io::Error::from(io::ErrorKind::Unsupported))
    }
    pub(super) fn multicast_hops_v6(_: &UdpSocket, _: u8) -> io::Result<()> {
        Err(io::Error::from(io::ErrorKind::Unsupported))
    }
}
