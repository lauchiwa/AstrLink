//! Turns a pasted site session into the closed credential envelope Core
//! accepts, so connecting an account needs no in-app login window.
//!
//! This is the only conversion path. It mirrors
//! `core/internal/forkcheckin/network_credential.go`
//! (`validateNetworkCredential`, `decodeNetworkCredential`, `safeNetworkPath`)
//! field for field: Core decodes with `DisallowUnknownFields`, so an extra or
//! renamed key is rejected rather than ignored.
//!
//! Only cookies are produced. A pasted bearer carries no reliable expiry, and
//! inventing `bearer_expires` would keep sending a stale token, so bearer
//! import is deliberately unsupported.

use base64::Engine as _;
use serde::Serialize;
use std::collections::HashSet;
use std::fmt;
use zeroize::Zeroizing;

/// Mirrors `forkcheckin.maxSessionCookies`.
const MAX_COOKIES: usize = 32;
/// Mirrors `forkcheckin.MaxCredentialBytes` (64 KiB decoded).
const MAX_DECODED_BYTES: usize = 64 << 10;
/// Paste input bound. Generous against the decoded cap while still refusing a
/// whole file dropped into the box.
const MAX_INPUT_BYTES: usize = MAX_DECODED_BYTES;

/// A base64 envelope ready for the `complete` call. The inner value is zeroed
/// on drop and never printed.
pub struct ImportedCredential(Zeroizing<String>);

impl ImportedCredential {
    /// Borrows the base64 for the request body. Callers must not copy it into
    /// a log, an event or an error.
    pub fn expose(&self) -> &str {
        &self.0
    }
}

impl fmt::Debug for ImportedCredential {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("[private check-in credential]")
    }
}

impl Serialize for ImportedCredential {
    fn serialize<S: serde::Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        serializer.serialize_str(self.expose())
    }
}

/// Why a paste was refused. Variants carry no input fragment, so neither the
/// message nor a wrapped error can echo a cookie name or value.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ImportError {
    /// No cookie pair was found.
    Empty,
    /// More than `MAX_COOKIES` pairs.
    TooMany,
    /// A pair, a name or a value is not a usable cookie.
    Malformed,
    /// The same name repeats for the same path.
    Duplicate,
    /// The paste, or the envelope it produces, exceeds the Core bound.
    Oversized,
}

impl ImportError {
    /// A message safe to surface in the UI.
    pub fn message(self) -> &'static str {
        match self {
            Self::Empty => "no cookie was found in the pasted text",
            Self::TooMany => "a session may carry at most 32 cookies",
            Self::Malformed => "the pasted text is not a cookie header or name/value list",
            Self::Duplicate => "the pasted text repeats a cookie name",
            Self::Oversized => "the pasted session is too large",
        }
    }
}

impl fmt::Display for ImportError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.message())
    }
}

impl std::error::Error for ImportError {}

/// Mirrors `forkcheckin.SessionCookie`. Field names and `omitempty` behaviour
/// must match the Go tags exactly.
#[derive(Serialize)]
struct SessionCookie {
    name: String,
    value: String,
    #[serde(skip_serializing_if = "str::is_empty")]
    domain: &'static str,
    path: &'static str,
    secure: bool,
    http_only: bool,
}

/// Mirrors `forkcheckin.NetworkCredential`. `bearer`, `bearer_expires` and
/// `proxy` are omitted entirely: paste import never produces them.
#[derive(Serialize)]
struct Envelope {
    version: u8,
    cookies: Vec<SessionCookie>,
}

/// True when the byte may appear in a cookie name, per RFC 6265 token rules.
/// Go's `http.Cookie.Valid` rejects anything else.
fn is_cookie_name_byte(byte: u8) -> bool {
    byte.is_ascii_alphanumeric()
        || matches!(
            byte,
            b'!' | b'#'
                | b'$'
                | b'%'
                | b'&'
                | b'\''
                | b'*'
                | b'+'
                | b'-'
                | b'.'
                | b'^'
                | b'_'
                | b'`'
                | b'|'
                | b'~'
        )
}

/// True when the byte may appear in a cookie value. Go rejects control bytes,
/// whitespace, quotes, comma, semicolon and backslash.
fn is_cookie_value_byte(byte: u8) -> bool {
    matches!(byte, 0x21 | 0x23..=0x2B | 0x2D..=0x3A | 0x3C..=0x5B | 0x5D..=0x7E)
}

/// Splits one `name=value` pair. The value may contain `=`, so only the first
/// separator counts.
fn split_pair(entry: &str) -> Result<(&str, &str), ImportError> {
    let entry = entry.trim();
    let (name, value) = entry
        .split_once('=')
        .or_else(|| entry.split_once('\t'))
        .ok_or(ImportError::Malformed)?;
    Ok((name.trim(), value.trim()))
}

/// Converts pasted text into the closed envelope.
///
/// Accepts the single-line `Cookie:` request header form (`a=1; b=2`), a
/// line-per-cookie form (`a=1` or `a<TAB>1`), and the two mixed together,
/// because a copied header often arrives wrapped across lines with its
/// semicolons intact. `secure` is decided by the account's own origin scheme,
/// never by the paste. `domain` is left empty, which Core treats as
/// host-only; guessing a domain could widen where the cookie is sent.
/// `http_only` is always false, because anything a person can copy out of a
/// browser was readable to script by definition.
pub fn parse_pasted_cookies(
    text: &str,
    https_origin: bool,
) -> Result<ImportedCredential, ImportError> {
    if text.len() > MAX_INPUT_BYTES {
        return Err(ImportError::Oversized);
    }
    let mut cookies = Vec::new();
    let mut seen = HashSet::new();
    // A cookie value cannot contain `;`, `\r` or `\n`, so splitting on all of
    // them at once accepts every paste shape without ambiguity.
    for entry in text.split([';', '\r', '\n']) {
        if entry.trim().is_empty() {
            continue;
        }
        let (name, value) = split_pair(entry)?;
        if name.is_empty() || !name.bytes().all(is_cookie_name_byte) {
            return Err(ImportError::Malformed);
        }
        if !value.bytes().all(is_cookie_value_byte) {
            return Err(ImportError::Malformed);
        }
        // Core enforces the cookie prefixes rather than trusting the paste:
        // `__Secure-` requires Secure, and `__Host-` additionally requires an
        // empty domain and the root path. Both hold here only on an https
        // origin, so a prefixed cookie pasted for an http origin is refused
        // instead of being silently downgraded.
        let prefixed = name.starts_with("__Secure-") || name.starts_with("__Host-");
        if prefixed && !https_origin {
            return Err(ImportError::Malformed);
        }
        if cookies.len() == MAX_COOKIES {
            return Err(ImportError::TooMany);
        }
        // Root path for every cookie: a pasted header carries no path, and
        // "/" is both the widest safe scope and `safeNetworkPath`-clean.
        if !seen.insert(name.to_owned()) {
            return Err(ImportError::Duplicate);
        }
        cookies.push(SessionCookie {
            name: name.to_owned(),
            value: value.to_owned(),
            domain: "",
            path: "/",
            secure: https_origin,
            http_only: false,
        });
    }
    if cookies.is_empty() {
        return Err(ImportError::Empty);
    }
    let envelope = Envelope {
        version: 1,
        cookies,
    };
    let json = Zeroizing::new(serde_json::to_vec(&envelope).map_err(|_| ImportError::Malformed)?);
    if json.len() > MAX_DECODED_BYTES {
        return Err(ImportError::Oversized);
    }
    Ok(ImportedCredential(Zeroizing::new(
        base64::engine::general_purpose::STANDARD.encode(json.as_slice()),
    )))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn decode(credential: &ImportedCredential) -> serde_json::Value {
        let raw = base64::engine::general_purpose::STANDARD
            .decode(credential.expose())
            .unwrap();
        serde_json::from_slice(&raw).unwrap()
    }

    #[test]
    fn every_paste_shape_yields_the_same_envelope() {
        let header = parse_pasted_cookies("session=abc; csrf=def", true).unwrap();
        for equivalent in [
            "session=abc\ncsrf=def\n",
            "session\tabc\ncsrf\tdef",
            // A copied header wrapped across lines keeps its semicolons.
            "session=abc;\ncsrf=def",
            "  session=abc ;  csrf=def  ",
            "session=abc;\r\ncsrf=def",
        ] {
            assert_eq!(
                decode(&header),
                decode(&parse_pasted_cookies(equivalent, true).unwrap()),
                "{equivalent:?}"
            );
        }
    }

    #[test]
    fn the_envelope_carries_only_the_fields_core_accepts() {
        let credential = parse_pasted_cookies("session=abc", true).unwrap();
        // Core decodes with DisallowUnknownFields, so the key set must match
        // the Go json tags exactly and omit everything paste cannot know.
        assert_eq!(
            decode(&credential),
            serde_json::json!({
                "version": 1,
                "cookies": [{
                    "name": "session", "value": "abc",
                    "path": "/", "secure": true, "http_only": false,
                }],
            })
        );
    }

    #[test]
    fn an_http_origin_yields_insecure_cookies_without_a_prefix() {
        let credential = parse_pasted_cookies("session=abc", false).unwrap();
        let value = decode(&credential);
        assert_eq!(value["cookies"][0]["secure"], false);
        // A prefixed name cannot satisfy Core's rules over plain http, so it
        // is refused rather than stored as a cookie the site will not accept.
        for name in ["__Secure-s=1", "__Host-s=1"] {
            assert_eq!(
                parse_pasted_cookies(name, false).err(),
                Some(ImportError::Malformed),
                "{name}"
            );
        }
    }

    #[test]
    fn prefixed_names_keep_their_required_attributes() {
        let credential =
            parse_pasted_cookies("__Host-session=abc; __Secure-csrf=def", true).unwrap();
        let value = decode(&credential);
        for index in 0..2 {
            let cookie = &value["cookies"][index];
            assert_eq!(cookie["secure"], true, "{index}");
            assert_eq!(cookie["path"], "/", "{index}");
            assert!(cookie.get("domain").is_none(), "{index}");
        }
    }

    #[test]
    fn bounds_and_malformed_input_are_refused_by_kind() {
        let many = (0..=MAX_COOKIES)
            .map(|index| format!("c{index}=v"))
            .collect::<Vec<_>>()
            .join("; ");
        assert_eq!(
            parse_pasted_cookies(&many, true).err(),
            Some(ImportError::TooMany)
        );
        let at_limit = (0..MAX_COOKIES)
            .map(|index| format!("c{index}=v"))
            .collect::<Vec<_>>()
            .join("; ");
        assert!(parse_pasted_cookies(&at_limit, true).is_ok());
        assert_eq!(
            parse_pasted_cookies("a=1; a=2", true).err(),
            Some(ImportError::Duplicate)
        );
        assert_eq!(
            parse_pasted_cookies("", true).err(),
            Some(ImportError::Empty)
        );
        for blank in ["   ", "   ;  \n ", "\n\n", " ; ; "] {
            assert_eq!(
                parse_pasted_cookies(blank, true).err(),
                Some(ImportError::Empty),
                "{blank:?}"
            );
        }
        assert_eq!(
            parse_pasted_cookies(&"a".repeat(MAX_INPUT_BYTES + 1), true).err(),
            Some(ImportError::Oversized)
        );
        for bad in [
            "sessionabc",         // no separator
            "=abc",               // empty name
            "ses sion=abc",       // space in name
            "session=a b",        // space in value
            "session=a\"b",       // quote in value
            "session=a,b",        // comma in value
            "session=a\\b",       // backslash in value
            "session=a\u{7f}b",   // control byte in value
            "sessi\u{00e9}n=abc", // non-ASCII name
        ] {
            assert_eq!(
                parse_pasted_cookies(bad, true).err(),
                Some(ImportError::Malformed),
                "{bad:?}"
            );
        }
    }

    #[test]
    fn a_value_may_contain_the_separator_character() {
        // JWT-shaped and base64 padded values are routine; only the first `=`
        // separates name from value.
        let credential = parse_pasted_cookies("session=eyJhbGci.payload.sig==", true).unwrap();
        assert_eq!(
            decode(&credential)["cookies"][0]["value"],
            "eyJhbGci.payload.sig=="
        );
    }

    #[test]
    fn neither_debug_nor_error_text_reveals_the_session() {
        let credential = parse_pasted_cookies("session=supersecretvalue", true).unwrap();
        let rendered = format!("{credential:?}");
        assert_eq!(rendered, "[private check-in credential]");
        assert!(!rendered.contains("supersecretvalue"));
        for error in [
            ImportError::Empty,
            ImportError::TooMany,
            ImportError::Malformed,
            ImportError::Duplicate,
            ImportError::Oversized,
        ] {
            let text = format!("{error} {error:?}");
            assert!(!text.contains("supersecretvalue"), "{error:?}");
        }
    }
}
