//! De proceseigenaar leest stdout/stderr zonder extra threads en behoudt SDK-logniveaus.
use std::{
    io::{self, Read},
    ops::{Deref, DerefMut},
    process::{Child, ChildStderr, ChildStdout},
};
use stulp_core::{Error, Result};
/// Minimumniveau van lokale pluginlogs; de keuze is onveranderlijk tijdens deze run.
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum Level {
    /// Alle diagnostiek.
    Debug,
    /// Gewone berichten plus waarschuwingen en fouten.
    Info,
    /// Waarschuwingen en fouten.
    Warn,
    /// Alleen fouten.
    Error,
}
impl Level {
    /// Dezelfde vier expliciete namen als de oorspronkelijke CLI.
    pub fn parse(name: &str) -> Result<Self> {
        match name {
            "debug" => Ok(Self::Debug),
            "info" => Ok(Self::Info),
            "warn" => Ok(Self::Warn),
            "error" => Ok(Self::Error),
            _ => Err(Error::Invalid(
                "unknown log level: use debug, info, warn or error",
            )),
        }
    }
    fn name(self) -> &'static str {
        match self {
            Self::Debug => "DEBUG",
            Self::Info => "INFO",
            Self::Warn => "WARN",
            Self::Error => "ERROR",
        }
    }
}
#[derive(Default)]
struct Lines {
    bytes: Vec<u8>,
    continuation: Option<Level>,
}
impl Lines {
    fn emit(&mut self, end: bool, output: &mut impl FnMut(Level, &str)) {
        let mut length = self.bytes.len();
        if !end && length > 0 {
            // Keep at most three bytes of an incomplete final UTF-8 scalar.
            // Earlier malformed output must not make us split a valid tail.
            let mut start = length - 1;
            while start > length.saturating_sub(4) && self.bytes[start] & 0xc0 == 0x80 {
                start -= 1;
            }
            if let Err(e) = std::str::from_utf8(&self.bytes[start..])
                && e.error_len().is_none()
            {
                length = start + e.valid_up_to();
            }
        }
        let line = String::from_utf8_lossy(&self.bytes[..length]);
        let line = if end {
            line.trim_end_matches('\r')
        } else {
            &line
        };
        if !line.is_empty() {
            let (level, message) = if let Some(level) = self.continuation {
                (level, line)
            } else {
                line.split_once('\t')
                    .and_then(|(level, message)| {
                        Level::parse(level).ok().map(|level| (level, message))
                    })
                    .unwrap_or((Level::Info, line))
            };
            output(level, message);
            self.continuation = Some(level);
        }
        let remaining = self.bytes.len() - length;
        self.bytes.copy_within(length.., 0);
        self.bytes.truncate(remaining);
        if end {
            self.continuation = None;
        }
    }
    fn feed(&mut self, bytes: &[u8], output: &mut impl FnMut(Level, &str)) -> Result {
        for b in bytes {
            if *b == b'\n' {
                self.emit(true, output);
            } else {
                if self.bytes.len() == 8192 {
                    self.emit(false, output);
                }
                self.bytes.try_reserve(1).map_err(|_| Error::Memory)?;
                self.bytes.push(*b);
            }
        }
        Ok(())
    }
    fn read(&mut self, stream: &mut impl Read, output: &mut impl FnMut(Level, &str)) -> Result {
        let mut buffer = [0; 4096];
        for _ in 0..32 {
            match stream.read(&mut buffer) {
                Ok(0) => {
                    self.emit(true, output);
                    break;
                }
                Ok(n) => self.feed(&buffer[..n], output)?,
                Err(e) if e.kind() == io::ErrorKind::WouldBlock => break,
                Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                Err(_) => {
                    self.emit(true, output);
                    return Err(Error::Invalid("cannot read plugin output"));
                }
            }
        }
        Ok(())
    }
}
pub(crate) struct Running {
    child: Child,
    stdout: ChildStdout,
    stderr: ChildStderr,
    output: Lines,
    errors: Lines,
}
impl Running {
    pub(crate) fn capture(mut child: Child) -> Result<Self> {
        let pipes = (|| {
            let out = child
                .stdout
                .take()
                .ok_or(Error::Invalid("plugin stdout missing"))?;
            let err = child
                .stderr
                .take()
                .ok_or(Error::Invalid("plugin stderr missing"))?;
            stulp_platform::socket::nonblocking(&out)
                .map_err(|_| Error::Invalid("cannot poll plugin stdout"))?;
            stulp_platform::socket::nonblocking(&err)
                .map_err(|_| Error::Invalid("cannot poll plugin stderr"))?;
            Ok((out, err))
        })();
        match pipes {
            Ok((stdout, stderr)) => Ok(Self {
                child,
                stdout,
                stderr,
                output: Lines::default(),
                errors: Lines::default(),
            }),
            Err(e) => {
                let _ = child.kill();
                let _ = child.wait();
                Err(e)
            }
        }
    }
    pub(crate) fn logs(&mut self, app: &str, minimum: Level) -> Result {
        let mut emit = |level: Level, message: &str| {
            if level >= minimum {
                eprintln!("level={} app={} msg={:?}", level.name(), app, message);
            }
        };
        self.output.read(&mut self.stdout, &mut emit)?;
        self.errors.read(&mut self.stderr, &mut emit)
    }
}
impl Deref for Running {
    type Target = Child;
    fn deref(&self) -> &Child {
        &self.child
    }
}
impl DerefMut for Running {
    fn deref_mut(&mut self) -> &mut Child {
        &mut self.child
    }
}
impl Drop for Running {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}
#[cfg(test)]
#[allow(clippy::unwrap_used)]
mod tests {
    use super::*;
    #[test]
    fn fragmented_and_unterminated_logs_keep_the_original_go_levels() {
        let mut lines = Lines::default();
        let mut messages = Vec::new();
        let mut emit = |level, text: &str| messages.push((level, text.to_owned()));
        for part in [
            b"debu".as_slice(),
            b"g\thidden\nerror\tlast words",
            b"\nplain library output\n\nwarn\ttail",
        ] {
            lines.feed(part, &mut emit).unwrap();
        }
        lines.emit(true, &mut emit);
        assert_eq!(messages.len(), 4);
        assert!(messages[0].0 == Level::Debug);
        assert!(messages[1].0 == Level::Error);
        assert!(messages[2].0 == Level::Info);
        assert!(messages[3].0 == Level::Warn);
        assert_eq!(messages[3].1, "tail");
        assert!(Level::parse("warning").is_err());
    }
    #[test]
    fn long_lines_are_chunked_without_losing_the_error_level() {
        let text = "x".repeat(20000);
        let mut lines = Lines::default();
        let mut got = String::new();
        let mut emit = |level, message: &str| {
            assert!(level == Level::Error);
            got.push_str(message);
        };
        lines.feed(b"error\t", &mut emit).unwrap();
        lines.feed(text.as_bytes(), &mut emit).unwrap();
        lines.emit(true, &mut emit);
        assert_eq!(got, text);
    }
    #[test]
    fn utf8_and_carriage_returns_survive_every_chunk_boundary() {
        for character in ["é", "界", "🦀"] {
            for offset in 0..4 {
                let text = format!(
                    "{}\r{}tail",
                    "x".repeat(8183 + offset),
                    character.repeat(8193)
                );
                let mut lines = Lines::default();
                let mut got = String::new();
                let mut emit = |level, message: &str| {
                    assert!(level == Level::Warn);
                    got.push_str(message);
                };
                lines.feed(b"warn\t", &mut emit).unwrap();
                for part in text.as_bytes().chunks(17) {
                    lines.feed(part, &mut emit).unwrap();
                    assert!(lines.bytes.len() <= 8192);
                }
                lines.feed(b"\r\n", &mut emit).unwrap();
                assert_eq!(got, text);
            }
        }
        let mut lines = Lines::default();
        let mut got = String::new();
        let mut emit = |_, message: &str| got.push_str(message);
        let mut bytes = vec![b'x'; 8191];
        bytes[0] = 0xff;
        bytes.extend_from_slice("🦀".as_bytes());
        lines.feed(&bytes, &mut emit).unwrap();
        lines.emit(true, &mut emit);
        assert_eq!(got, String::from_utf8_lossy(&bytes));
    }
}
