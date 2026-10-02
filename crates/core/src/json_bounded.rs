//! Dezelfde Hop-parser met een expliciete grens voor documenten en appframes.
// Afgeleid van xinix00/hop v3.0.0-alpha.10, commit 998ebdc, types/src/json.rs.
// Oorspronkelijke MIT-licentie met Commons Clause staat in HOP-LICENSE.
// Aanpassingen: caller-grens en openbare Object::push in plaats van het private pairs-veld.
use alloc::{string::String, vec::Vec};
use hop_types::{
    Error, Name, Result,
    json::{MAX_DEPTH, Number, Object, Value},
    try_push, try_push_str,
};
pub(super) fn parse(input: &[u8], limit: usize) -> Result<Value> {
    if input.len() > limit {
        return Err(Error::TooLarge {
            len: input.len(),
            max: limit,
        });
    }
    let mut p = Parser { b: input, pos: 0 };
    p.skip_ws();
    let v = p.value(0)?;
    p.skip_ws();
    if p.pos < p.b.len() {
        return Err(Error::Trailing { offset: p.pos });
    }
    Ok(v)
}

struct Parser<'a> {
    b: &'a [u8],
    pos: usize,
}

impl Parser<'_> {
    fn peek(&self) -> Option<u8> {
        self.b.get(self.pos).copied()
    }

    fn err(&self, expected: &'static str) -> Error {
        Error::Syntax {
            offset: self.pos,
            expected,
        }
    }

    fn skip_ws(&mut self) {
        while let Some(b' ' | b'\t' | b'\n' | b'\r') = self.peek() {
            self.pos += 1;
        }
    }

    fn eat(&mut self, c: u8, expected: &'static str) -> Result {
        if self.peek() == Some(c) {
            self.pos += 1;
            Ok(())
        } else {
            Err(self.err(expected))
        }
    }

    fn literal(&mut self, word: &'static [u8], v: Value) -> Result<Value> {
        let end = self.pos.saturating_add(word.len());
        if self.b.get(self.pos..end) == Some(word) {
            self.pos = end;
            Ok(v)
        } else {
            Err(self.err("a value"))
        }
    }

    fn value(&mut self, depth: usize) -> Result<Value> {
        if depth >= MAX_DEPTH {
            return Err(Error::TooDeep {
                offset: self.pos,
                max: MAX_DEPTH,
            });
        }
        match self.peek() {
            Some(b'{') => self.object(depth),
            Some(b'[') => self.array(depth),
            Some(b'"') => Ok(Value::String(self.string()?)),
            Some(b't') => self.literal(b"true", Value::Bool(true)),
            Some(b'f') => self.literal(b"false", Value::Bool(false)),
            Some(b'n') => self.literal(b"null", Value::Null),
            Some(b'-' | b'0'..=b'9') => Ok(Value::Number(self.number()?)),
            _ => Err(self.err("a value")),
        }
    }

    fn object(&mut self, depth: usize) -> Result<Value> {
        self.pos += 1;
        let mut obj = Object::new();
        self.skip_ws();
        if self.peek() == Some(b'}') {
            self.pos += 1;
            return Ok(Value::Object(obj));
        }
        loop {
            self.skip_ws();
            if self.peek() != Some(b'"') {
                return Err(self.err("a string key"));
            }
            let key = self.string()?;
            if obj.get(&key).is_some() {
                return Err(Error::DuplicateKey {
                    key: Name::new(&key),
                });
            }
            self.skip_ws();
            self.eat(b':', "':'")?;
            self.skip_ws();
            let v = self.value(depth + 1)?;
            obj.push(&key, v)?;
            self.skip_ws();
            match self.peek() {
                Some(b',') => self.pos += 1,
                Some(b'}') => {
                    self.pos += 1;
                    return Ok(Value::Object(obj));
                }
                _ => return Err(self.err("',' or '}'")),
            }
        }
    }

    fn array(&mut self, depth: usize) -> Result<Value> {
        self.pos += 1;
        let mut arr = Vec::new();
        self.skip_ws();
        if self.peek() == Some(b']') {
            self.pos += 1;
            return Ok(Value::Array(arr));
        }
        loop {
            self.skip_ws();
            let v = self.value(depth + 1)?;
            try_push(&mut arr, v)?;
            self.skip_ws();
            match self.peek() {
                Some(b',') => self.pos += 1,
                Some(b']') => {
                    self.pos += 1;
                    return Ok(Value::Array(arr));
                }
                _ => return Err(self.err("',' or ']'")),
            }
        }
    }

    fn hex4(&mut self) -> Result<u32> {
        let mut v = 0u32;
        for _ in 0..4 {
            let d = match self.peek() {
                Some(c @ b'0'..=b'9') => c - b'0',
                Some(c @ b'a'..=b'f') => c - b'a' + 10,
                Some(c @ b'A'..=b'F') => c - b'A' + 10,
                _ => return Err(self.err("four hex digits")),
            };
            v = (v << 4) | u32::from(d);
            self.pos += 1;
        }
        Ok(v)
    }

    fn escape(&mut self, out: &mut String) -> Result {
        let c = match self.peek() {
            Some(b'"') => '"',
            Some(b'\\') => '\\',
            Some(b'/') => '/',
            Some(b'b') => '\u{8}',
            Some(b'f') => '\u{c}',
            Some(b'n') => '\n',
            Some(b'r') => '\r',
            Some(b't') => '\t',
            Some(b'u') => {
                self.pos += 1;
                return self.unicode(out);
            }
            _ => return Err(self.err("an escape")),
        };
        self.pos += 1;
        push_char(out, c)
    }

    fn unicode(&mut self, out: &mut String) -> Result {
        let hi = self.hex4()?;
        let cp = if (0xD800..0xDC00).contains(&hi) {
            // Een surrogaatpaar: de tweede helft moet direct volgen.
            if self.b.get(self.pos..self.pos.saturating_add(2)) != Some(b"\\u") {
                return Err(self.err("a low surrogate"));
            }
            self.pos += 2;
            let lo = self.hex4()?;
            if !(0xDC00..0xE000).contains(&lo) {
                return Err(self.err("a low surrogate"));
            }
            0x10000 + ((hi - 0xD800) << 10) + (lo - 0xDC00)
        } else {
            hi
        };
        // Een losse lage surrogaat is geen teken; Go maakt er U+FFFD van.
        let c = char::from_u32(cp).unwrap_or('\u{FFFD}');
        push_char(out, c)
    }

    fn string(&mut self) -> Result<String> {
        self.pos += 1;
        let mut out = String::new();
        loop {
            let start = self.pos;
            while let Some(c) = self.peek() {
                if c == b'"' || c == b'\\' || c < 0x20 {
                    break;
                }
                self.pos += 1;
            }
            let run = self.b.get(start..self.pos).unwrap_or_default();
            let run = core::str::from_utf8(run).map_err(|e| Error::Syntax {
                offset: start + e.valid_up_to(),
                expected: "UTF-8",
            })?;
            try_push_str(&mut out, run)?;
            match self.peek() {
                Some(b'"') => {
                    self.pos += 1;
                    return Ok(out);
                }
                Some(b'\\') => {
                    self.pos += 1;
                    self.escape(&mut out)?;
                }
                _ => return Err(self.err("a closing '\"'")),
            }
        }
    }

    fn digits(&mut self) -> usize {
        let start = self.pos;
        while let Some(b'0'..=b'9') = self.peek() {
            self.pos += 1;
        }
        self.pos - start
    }

    fn number(&mut self) -> Result<Number> {
        let start = self.pos;
        if self.peek() == Some(b'-') {
            self.pos += 1;
        }
        match self.peek() {
            Some(b'0') => self.pos += 1,
            Some(b'1'..=b'9') => {
                self.digits();
            }
            _ => return Err(self.err("a digit")),
        }
        let mut float = false;
        if self.peek() == Some(b'.') {
            self.pos += 1;
            float = true;
            if self.digits() == 0 {
                return Err(self.err("a digit"));
            }
        }
        if let Some(b'e' | b'E') = self.peek() {
            self.pos += 1;
            float = true;
            if let Some(b'+' | b'-') = self.peek() {
                self.pos += 1;
            }
            if self.digits() == 0 {
                return Err(self.err("a digit"));
            }
        }
        let text = self.b.get(start..self.pos).unwrap_or_default();
        // Alleen ASCII-cijfers en tekens gezien, dus dit is geldige UTF-8.
        let text = core::str::from_utf8(text).map_err(|_| self.err("a number"))?;
        if !float {
            if let Ok(v) = text.parse::<u64>() {
                return Ok(Number::Uint(v));
            }
            if let Ok(v) = text.parse::<i64>() {
                return Ok(Number::Int(v));
            }
        }
        text.parse::<f64>()
            .map(Number::Float)
            .map_err(|_| Error::Syntax {
                offset: start,
                expected: "a number",
            })
    }
}

fn push_char(out: &mut String, c: char) -> Result {
    out.try_reserve(c.len_utf8())
        .map_err(|_| Error::OutOfMemory)?;
    out.push(c);
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn parser_retains_hop_errors_types_and_unicode_contract() {
        for text in [
            r#"{"a":[0,-1,18446744073709551615,1.25,true,null,"\uD83D\uDE03"]}"#,
            r#"{"a":1,"a":2}"#,
            r#"[1,]"#,
            r#"{"\u0061":1,"a":2}"#,
            r#""\uD800""#,
            "01",
            "true false",
            "{",
            "[]",
        ] {
            let original = hop_types::json::parse(text.as_bytes());
            let bounded = parse(text.as_bytes(), 65536);
            assert_eq!(original, bounded, "{text}");
        }
        let data = br#"{"key":1}"#;
        assert!(matches!(
            parse(data, 8),
            Err(Error::TooLarge { len: 9, max: 8 })
        ));
        let deep = "[".repeat(MAX_DEPTH + 1) + &"]".repeat(MAX_DEPTH + 1);
        assert_eq!(
            parse(deep.as_bytes(), 65536),
            hop_types::json::parse(deep.as_bytes())
        );
    }
    #[test]
    fn large_document_roundtrips_while_api_parser_stays_bounded() {
        let payload = "x".repeat(2 << 20);
        let data =
            alloc::format!(r#"{{"version":2,"appState":{{"test":{{"payload":"{payload}"}}}}}}"#);
        assert!(hop_types::json::parse(data.as_bytes()).is_err());
        let document = crate::document::Document::decode(data.as_bytes()).unwrap();
        let encoded = document.encode().unwrap();
        let reread = crate::document::Document::decode(encoded.as_bytes()).unwrap();
        assert!(crate::json::equal(document.root(), reread.root()));
        let slot = crate::slots::encode(1, encoded.as_bytes()).unwrap();
        assert_eq!(
            crate::slots::decode(&slot).unwrap().payload,
            encoded.as_bytes()
        );
    }
}
