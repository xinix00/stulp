//! Gregoriaanse kalender zonder OS-klok; epochseconden zijn altijd UTC.
use stulp_core::{Error, Result};
/// Een lokale kalenderdatum, met een afzonderlijke UTC-offset voor dubbele DST-minuten.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Civil {
    /// Volledig jaartal.
    pub year: i32,
    /// Maand 1..12.
    pub month: u8,
    /// Dag 1..31.
    pub day: u8,
    /// Uur 0..23.
    pub hour: u8,
    /// Minuut 0..59.
    pub minute: u8,
    /// UTC-offset in seconden.
    pub offset: i32,
}
/// Een Gregoriaans schrikkeljaar.
pub fn leap(year: i32) -> bool {
    year % 4 == 0 && (year % 100 != 0 || year % 400 == 0)
}
/// Aantal dagen per maand; ongeldige maanden leveren nul.
pub fn month_days(year: i32, month: u8) -> u8 {
    match month {
        1 | 3 | 5 | 7 | 8 | 10 | 12 => 31,
        4 | 6 | 9 | 11 => 30,
        2 => {
            if leap(year) {
                29
            } else {
                28
            }
        }
        _ => 0,
    }
}
/// Middernacht op een datum, gerekend als UTC, in epochseconden.
pub fn midnight(year: i32, month: u8, day: u8) -> Result<i64> {
    if !(1..=9999).contains(&year) || day == 0 || day > month_days(year, month) {
        return Err(Error::Invalid("calendar date out of range"));
    }
    let y = i64::from(year) - i64::from(month <= 2);
    let era = y.div_euclid(400);
    let yo = y - era * 400;
    let m = i64::from(month) + if month > 2 { -3 } else { 9 };
    let doy = (153 * m + 2) / 5 + i64::from(day) - 1;
    Ok((era * 146097 + yo * 365 + yo / 4 - yo / 100 + doy - 719468) * 86400)
}
/// Splitst epochseconden met een expliciete tijdzone-offset; mutatie van TZ is niet nodig.
pub fn civil(unix: i64, offset: i32) -> Result<Civil> {
    let local = unix.checked_add(i64::from(offset)).ok_or(Error::Full)?;
    let day = local.div_euclid(86400);
    let z = day.checked_add(719468).ok_or(Error::Full)?;
    let era = z.div_euclid(146097);
    let doe = z - era * 146097;
    let yo = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let doy = doe - (365 * yo + yo / 4 - yo / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = mp + if mp < 10 { 3 } else { -9 };
    let y = yo + era * 400 + i64::from(m <= 2);
    if !(1..=9999).contains(&y) {
        return Err(Error::Invalid("clock date out of range"));
    }
    Ok(Civil {
        year: y as i32,
        month: m as u8,
        day: d as u8,
        hour: (local.rem_euclid(86400) / 3600) as u8,
        minute: (local.rem_euclid(3600) / 60) as u8,
        offset,
    })
}
