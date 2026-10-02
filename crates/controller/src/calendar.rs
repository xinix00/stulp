//! De NOAA-berekening volgt dezelfde stappen en dagkeuze als de Go Flow-engine.
use crate::timezone::Timezone;
use stulp_core::{Error, Result};
use stulp_runtime::{
    calendar::{self, Civil},
    schedule::Clock,
};
impl Clock for Timezone {
    fn local(&self, unix: i64) -> Result<Civil> {
        self.local(unix)
    }
    fn solar(&self, date: Civil, lat: f64, lon: f64, rise: bool) -> Result<Option<i64>> {
        let midnight = calendar::midnight(date.year, date.month, date.day)?;
        let day = (midnight - calendar::midnight(date.year, 1, 1)?) / 86400 + 1;
        let lng = lon / 15.0;
        let hour = if rise { 6.0 } else { 18.0 };
        let t = day as f64 + (hour - lng) / 24.0;
        let mean = 0.9856 * t - 3.289;
        let longitude = (mean
            + 1.916 * libm::sin(mean.to_radians())
            + 0.020 * libm::sin((2.0 * mean).to_radians())
            + 282.634)
            .pipe_rem(360.0);
        let mut ra = libm::atan(0.91764 * libm::tan(longitude.to_radians()))
            .to_degrees()
            .pipe_rem(360.0);
        ra += libm::floor(longitude / 90.0) * 90.0 - libm::floor(ra / 90.0) * 90.0;
        ra /= 15.0;
        let sin_dec = 0.39782 * libm::sin(longitude.to_radians());
        let cos_dec = libm::cos(libm::asin(sin_dec));
        let cos_h = (libm::cos(90.833_f64.to_radians()) - sin_dec * libm::sin(lat.to_radians()))
            / (cos_dec * libm::cos(lat.to_radians()));
        if !(-1.0..=1.0).contains(&cos_h) {
            return Ok(None);
        }
        let mut h = libm::acos(cos_h).to_degrees();
        if rise {
            h = 360.0 - h;
        }
        h /= 15.0;
        let local = h + ra - 0.06571 * t - 6.622;
        let ut = (local - lng).pipe_rem(24.0);
        Ok(Some(
            midnight
                .checked_add((ut * 3600.0) as i64)
                .ok_or(Error::Full)?,
        ))
    }
}

trait Remainder {
    fn pipe_rem(self, divisor: f64) -> f64;
}
impl Remainder for f64 {
    fn pipe_rem(self, divisor: f64) -> f64 {
        let r = libm::fmod(self, divisor);
        if r < 0.0 { r + divisor } else { r }
    }
}
