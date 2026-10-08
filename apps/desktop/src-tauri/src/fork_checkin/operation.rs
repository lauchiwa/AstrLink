//! Closed operation bridge for the check-in extension (task C08).
//!
//! The main window names one operation from `checkin.openapi.yaml`; the host
//! maps it to a fixed method, route and validated body. A caller can supply
//! no URL, method, header or path fragment, and unknown operations or fields
//! are refused before anything is sent.
//!
//! Every write carries a `request_id` fixed for one logical action. The host
//! generates it unless the caller resumes an earlier action with the id it
//! was given, so a retried action, including the transport's own automatic
//! retry, replays Core's stored receipt instead of acting twice.
//!
//! Completing an authorization sends a session the person captured in their
//! own browser. The main window hands over the raw paste once; the host
//! converts it to the closed envelope and the envelope never travels back.

use super::credential_import::{parse_pasted_cookies, ImportedCredential};
use reqwest::Method;
use serde::{Deserialize, Serialize};
use std::{collections::BTreeSet, fmt::Write as _};

/// The extension's private namespace; see `CheckinExtensionPath` in Core.
pub const API_PREFIX: &str = "/control/v1/extensions/checkin";
/// The wire version this desktop speaks. A Core that reports another one is
/// treated as unsupported rather than sent requests shaped for this one.
pub const PROTOCOL_VERSION: u64 = 1;
const MAX_PAGE_LIMIT: u32 = 100;
const MAX_CURSOR_BYTES: usize = 256;
const MAX_BATCH_ACCOUNTS: usize = 25;
const MAX_BOUND_SERVICES: usize = 32;
// Core's MaxDashboardURLLen; the proxy address shares the bound.
const MAX_URL_BYTES: usize = 2048;
// The contract's `claimed_user_id` bound.
const MAX_CLAIMED_USER_BYTES: usize = 128;
const MAX_TIME_ZONE_BYTES: usize = 64;
// JavaScript integers are exact up to 2^53 - 1.
const MAX_REVISION: u64 = (1 << 53) - 1;

/// One extension operation, tagged by `op`.
#[derive(Deserialize)]
#[serde(tag = "op", rename_all = "snake_case", deny_unknown_fields)]
pub enum CheckinOperation {
    Status {},
    GetSettings {},
    UpdateSettings {
        request_id: Option<String>,
        enabled: bool,
    },
    ListAccounts {
        limit: Option<u32>,
        cursor: Option<String>,
    },
    GetAccount {
        account_id: String,
    },
    CreateAccount {
        request_id: Option<String>,
        dashboard_base_url: String,
        time_zone: String,
        network: Option<Network>,
    },
    UpdateAccount {
        account_id: String,
        request_id: Option<String>,
        expected_revision: u64,
        dashboard_base_url: Option<String>,
        time_zone: Option<String>,
        network: Option<Network>,
        automatic: Option<bool>,
        bound_services: Option<Vec<String>>,
    },
    DeleteAccount {
        account_id: String,
        request_id: Option<String>,
        expected_revision: u64,
    },
    ListJobs {
        limit: Option<u32>,
        cursor: Option<String>,
        account_id: Option<String>,
    },
    GetJob {
        job_id: String,
    },
    CreateJob {
        request_id: Option<String>,
        action: JobAction,
        accounts: Vec<String>,
        expected_revision: Option<u64>,
    },
    CancelJob {
        job_id: String,
        request_id: Option<String>,
    },
    BeginAuthorization {
        request_id: Option<String>,
        account_id: String,
        expected_revision: u64,
    },
    /// Finishes a connection with a session the person captured in their own
    /// browser. `pasted_cookies` is the raw paste; the host converts it and
    /// the envelope never travels back to the window.
    CompleteAuthorization {
        session_id: String,
        request_id: Option<String>,
        pasted_cookies: String,
        /// The account's own dashboard address, used only to decide whether
        /// the cookies are marked `Secure`. Core re-reads the account's
        /// stored address and remains the authority.
        dashboard_base_url: String,
        expected_revision: Option<u64>,
        /// Who the person believes they signed in as. Passed through
        /// untouched: Core asks the site and stores the site's answer.
        claimed_user_id: Option<String>,
    },
}

/// Egress for one account. Proxy credentials are sealed with the session, so
/// `proxy_url` carries none; Core refuses one that does.
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Network {
    mode: NetworkMode,
    #[serde(skip_serializing_if = "Option::is_none")]
    proxy_url: Option<String>,
}
#[derive(Clone, Copy, Deserialize, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum NetworkMode {
    Direct,
    System,
    Custom,
}

#[derive(Clone, Copy, Deserialize, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum JobAction {
    CheckIn,
    StatusRefresh,
}

// Request bodies, in the contract's field order. Absent optional fields are
// omitted rather than sent as null.

#[derive(Serialize)]
struct SettingsBody {
    request_id: String,
    enabled: bool,
}

#[derive(Serialize)]
struct AccountDraftBody {
    request_id: String,
    dashboard_base_url: String,
    time_zone: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    network: Option<Network>,
}

#[derive(Serialize)]
struct AccountUpdateBody {
    request_id: String,
    expected_revision: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    dashboard_base_url: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    time_zone: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    network: Option<Network>,
    #[serde(skip_serializing_if = "Option::is_none")]
    automatic: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    bound_services: Option<Vec<String>>,
}
#[derive(Serialize)]
struct JobBody {
    request_id: String,
    action: JobAction,
    accounts: Vec<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    expected_revision: Option<u64>,
}

#[derive(Serialize)]
struct CancelBody {
    request_id: String,
}

#[derive(Serialize)]
struct AuthorizationBody {
    request_id: String,
    account_id: String,
    expected_revision: u64,
}

/// Mirrors `AuthorizationCompletion`. `credential` is write-only in the
/// contract and this struct is never deserialized or logged.
#[derive(Serialize)]
struct CompletionBody {
    request_id: String,
    credential: ImportedCredential,
    #[serde(skip_serializing_if = "Option::is_none")]
    expected_revision: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    claimed_user_id: Option<String>,
}

impl CheckinOperation {
    /// Validates the operation and fixes its `request_id`, so every attempt to
    /// send it is the same logical action.
    pub fn prepare(self) -> Result<PreparedRequest, String> {
        match self {
            Self::Status {} => Ok(PreparedRequest::read(Kind::Status, "/status".into())),
            Self::GetSettings {} => Ok(PreparedRequest::read(Kind::Fixed, "/settings".into())),
            Self::UpdateSettings {
                request_id,
                enabled,
            } => {
                let request_id = resolve_request_id(request_id)?;
                let body = SettingsBody {
                    request_id: request_id.clone(),
                    enabled,
                };
                PreparedRequest::write(
                    Kind::Fixed,
                    Method::PUT,
                    "/settings".into(),
                    request_id,
                    &body,
                )
            }
            Self::ListAccounts { limit, cursor } => {
                let query = page_query(limit, cursor)?;
                Ok(PreparedRequest::read(Kind::Fixed, "/accounts".into()).with_query(query))
            }
            Self::GetAccount { account_id } => {
                check_account_id(&account_id, "account_id")?;
                Ok(PreparedRequest::read(
                    Kind::Lookup,
                    format!("/accounts/{account_id}"),
                ))
            }
            Self::CreateAccount {
                request_id,
                dashboard_base_url,
                time_zone,
                network,
            } => {
                check_url(&dashboard_base_url, "dashboard_base_url")?;
                check_text(&time_zone, "time_zone", MAX_TIME_ZONE_BYTES)?;
                if let Some(network) = &network {
                    check_network(network)?;
                }
                let request_id = resolve_request_id(request_id)?;
                let body = AccountDraftBody {
                    request_id: request_id.clone(),
                    dashboard_base_url,
                    time_zone,
                    network,
                };
                PreparedRequest::write(
                    Kind::Fixed,
                    Method::POST,
                    "/accounts".into(),
                    request_id,
                    &body,
                )
            }
            Self::UpdateAccount {
                account_id,
                request_id,
                expected_revision,
                dashboard_base_url,
                time_zone,
                network,
                automatic,
                bound_services,
            } => {
                check_account_id(&account_id, "account_id")?;
                check_revision(expected_revision)?;
                if dashboard_base_url.is_none()
                    && time_zone.is_none()
                    && network.is_none()
                    && automatic.is_none()
                    && bound_services.is_none()
                {
                    return Err("the update names no field".into());
                }
                if let Some(url) = &dashboard_base_url {
                    check_url(url, "dashboard_base_url")?;
                }
                if let Some(zone) = &time_zone {
                    check_text(zone, "time_zone", MAX_TIME_ZONE_BYTES)?;
                }
                if let Some(network) = &network {
                    check_network(network)?;
                }
                if let Some(services) = &bound_services {
                    check_bound_services(services)?;
                }
                let request_id = resolve_request_id(request_id)?;
                let body = AccountUpdateBody {
                    request_id: request_id.clone(),
                    expected_revision,
                    dashboard_base_url,
                    time_zone,
                    network,
                    automatic,
                    bound_services,
                };
                let route = format!("/accounts/{account_id}");
                PreparedRequest::write(Kind::Lookup, Method::PATCH, route, request_id, &body)
            }
            Self::DeleteAccount {
                account_id,
                request_id,
                expected_revision,
            } => {
                check_account_id(&account_id, "account_id")?;
                check_revision(expected_revision)?;
                let request_id = resolve_request_id(request_id)?;
                // Delete takes its idempotency key in the query and no body.
                // An account that is already gone answers 204, so a 404 here
                // can only come from a Core without the extension.
                let query = vec![
                    ("request_id", request_id.clone()),
                    ("expected_revision", expected_revision.to_string()),
                ];
                Ok(PreparedRequest {
                    kind: Kind::Fixed,
                    method: Method::DELETE,
                    route: format!("/accounts/{account_id}"),
                    query,
                    body: None,
                    request_id: Some(request_id),
                })
            }
            Self::ListJobs {
                limit,
                cursor,
                account_id,
            } => {
                let mut query = page_query(limit, cursor)?;
                if let Some(account_id) = account_id {
                    check_account_id(&account_id, "account_id")?;
                    query.push(("account_id", account_id));
                }
                Ok(PreparedRequest::read(Kind::Fixed, "/jobs".into()).with_query(query))
            }
            Self::GetJob { job_id } => {
                check_job_id(&job_id)?;
                Ok(PreparedRequest::read(
                    Kind::Lookup,
                    format!("/jobs/{job_id}"),
                ))
            }
            Self::CreateJob {
                request_id,
                action,
                accounts,
                expected_revision,
            } => {
                check_job_accounts(&accounts, expected_revision)?;
                let request_id = resolve_request_id(request_id)?;
                let body = JobBody {
                    request_id: request_id.clone(),
                    action,
                    accounts,
                    expected_revision,
                };
                // A 404 here names an unknown account, not a missing route.
                PreparedRequest::write(
                    Kind::Lookup,
                    Method::POST,
                    "/jobs".into(),
                    request_id,
                    &body,
                )
            }
            Self::CancelJob { job_id, request_id } => {
                check_job_id(&job_id)?;
                let request_id = resolve_request_id(request_id)?;
                let body = CancelBody {
                    request_id: request_id.clone(),
                };
                let route = format!("/jobs/{job_id}/cancel");
                PreparedRequest::write(Kind::Lookup, Method::POST, route, request_id, &body)
            }
            Self::BeginAuthorization {
                request_id,
                account_id,
                expected_revision,
            } => {
                check_account_id(&account_id, "account_id")?;
                check_revision(expected_revision)?;
                let request_id = resolve_request_id(request_id)?;
                let body = AuthorizationBody {
                    request_id: request_id.clone(),
                    account_id,
                    expected_revision,
                };
                let route = "/authorizations".into();
                PreparedRequest::write(Kind::Lookup, Method::POST, route, request_id, &body)
            }
            Self::CompleteAuthorization {
                session_id,
                request_id,
                pasted_cookies,
                dashboard_base_url,
                expected_revision,
                claimed_user_id,
            } => {
                check_session_id(&session_id)?;
                check_url(&dashboard_base_url, "dashboard_base_url")?;
                if let Some(revision) = expected_revision {
                    check_revision(revision)?;
                }
                if let Some(user_id) = &claimed_user_id {
                    check_text(user_id, "claimed_user_id", MAX_CLAIMED_USER_BYTES)?;
                }
                // Plain http is the account's own choice and Core validates
                // it; only loopback reaches here in practice. Marking the
                // cookies Secure for an http site would stop them being sent.
                let secure = dashboard_base_url
                    .get(..6)
                    .unwrap_or_default()
                    .eq_ignore_ascii_case("https:");
                let credential = parse_pasted_cookies(&pasted_cookies, secure)
                    .map_err(|error| error.message().to_string())?;
                let request_id = resolve_request_id(request_id)?;
                let body = CompletionBody {
                    request_id: request_id.clone(),
                    credential,
                    expected_revision,
                    claimed_user_id,
                };
                let route = format!("/authorizations/{session_id}/complete");
                PreparedRequest::write(Kind::Lookup, Method::POST, route, request_id, &body)
            }
        }
    }
}

// Validators follow checkin.openapi.yaml. Core validates again and remains
// the authority for URL and zone semantics; these checks keep a malformed or
// hostile value from shaping the route at all. Errors name the field only.

fn check_account_id(value: &str, field: &str) -> Result<(), String> {
    let bytes = value.as_bytes();
    let valid = (3..=64).contains(&bytes.len())
        && bytes[0].is_ascii_lowercase()
        && bytes
            .iter()
            .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || *byte == b'_');
    if valid {
        Ok(())
    } else {
        Err(format!("{field} is not a check-in account id"))
    }
}
fn check_job_id(value: &str) -> Result<(), String> {
    let valid = (3..=64).contains(&value.len())
        && value
            .bytes()
            .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'_');
    if valid {
        Ok(())
    } else {
        Err("job_id is not a check-in job id".into())
    }
}

fn is_service_id(value: &str) -> bool {
    let bytes = value.as_bytes();
    (3..=96).contains(&bytes.len())
        && bytes[0].is_ascii_lowercase()
        && bytes.iter().all(|byte| {
            byte.is_ascii_lowercase() || byte.is_ascii_digit() || matches!(byte, b'_' | b'-')
        })
}

fn is_request_id(value: &str) -> bool {
    (8..=128).contains(&value.len())
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
}

/// `SessionId` shares `RequestId`'s shape in the contract. Validated here so
/// a hostile value cannot shape the route at all.
fn check_session_id(value: &str) -> Result<(), String> {
    if is_request_id(value) {
        Ok(())
    } else {
        Err("session_id must be 8-128 URL-safe characters".into())
    }
}

fn check_revision(revision: u64) -> Result<(), String> {
    if (1..=MAX_REVISION).contains(&revision) {
        Ok(())
    } else {
        Err("expected_revision must be a positive revision".into())
    }
}

fn check_text(value: &str, field: &str, max_bytes: usize) -> Result<(), String> {
    if value.is_empty() || value.len() > max_bytes || value.chars().any(char::is_control) {
        Err(format!("{field} must be 1-{max_bytes} bytes of text"))
    } else {
        Ok(())
    }
}

// Only the shape is checked here; Core decides whether the address is an
// acceptable relay or proxy.
fn check_url(value: &str, field: &str) -> Result<(), String> {
    check_text(value, field, MAX_URL_BYTES)?;
    let lower = value.get(..8).unwrap_or(value).to_ascii_lowercase();
    if lower.starts_with("https://") || lower.starts_with("http://") {
        Ok(())
    } else {
        Err(format!("{field} must be an absolute http(s) address"))
    }
}
fn check_network(network: &Network) -> Result<(), String> {
    match (network.mode, &network.proxy_url) {
        (NetworkMode::Custom, Some(proxy_url)) => crate::service_proxy::validate_proxy(
            &serde_json::json!({"mode": "custom", "url": proxy_url}),
            None,
            false,
        ),
        (NetworkMode::Custom, None) => Err("network.proxy_url is required for custom".into()),
        (_, Some(_)) => Err("network.proxy_url is allowed only for custom".into()),
        (_, None) => Ok(()),
    }
}

fn check_bound_services(services: &[String]) -> Result<(), String> {
    if services.len() > MAX_BOUND_SERVICES {
        return Err(format!(
            "bound_services names at most {MAX_BOUND_SERVICES} services"
        ));
    }
    if services.iter().all(|service| is_service_id(service)) {
        Ok(())
    } else {
        Err("bound_services must name service ids".into())
    }
}

fn check_job_accounts(accounts: &[String], expected_revision: Option<u64>) -> Result<(), String> {
    if accounts.is_empty() || accounts.len() > MAX_BATCH_ACCOUNTS {
        return Err(format!(
            "accounts must name 1 to {MAX_BATCH_ACCOUNTS} accounts"
        ));
    }
    let mut seen = BTreeSet::new();
    for account in accounts {
        check_account_id(account, "accounts")?;
        if !seen.insert(account.as_str()) {
            return Err("accounts must be distinct".into());
        }
    }
    match expected_revision {
        Some(_) if accounts.len() > 1 => {
            Err("expected_revision is allowed only for one account".into())
        }
        Some(revision) => check_revision(revision),
        None => Ok(()),
    }
}

fn page_query(
    limit: Option<u32>,
    cursor: Option<String>,
) -> Result<Vec<(&'static str, String)>, String> {
    let mut query = Vec::new();
    if let Some(limit) = limit {
        if !(1..=MAX_PAGE_LIMIT).contains(&limit) {
            return Err(format!("limit must be 1 to {MAX_PAGE_LIMIT}"));
        }
        query.push(("limit", limit.to_string()));
    }
    if let Some(cursor) = cursor {
        // Opaque, so it is percent-encoded when the path is built.
        if cursor.is_empty() || cursor.len() > MAX_CURSOR_BYTES {
            return Err("cursor is invalid".into());
        }
        query.push(("cursor", cursor));
    }
    Ok(query)
}
/// Resumes the caller's earlier action when it names one, otherwise starts a
/// new action with a fresh key.
fn resolve_request_id(resume: Option<String>) -> Result<String, String> {
    match resume {
        Some(request_id) if is_request_id(&request_id) => Ok(request_id),
        Some(_) => Err("request_id must be 8-128 URL-safe characters".into()),
        None => generate_request_id(),
    }
}

// 128 random bits as hex: no product name, timestamp or host detail.
fn generate_request_id() -> Result<String, String> {
    let mut random = [0_u8; 16];
    getrandom::getrandom(&mut random)
        .map_err(|error| format!("unable to generate a check-in request id: {error}"))?;
    let mut request_id = String::with_capacity(random.len() * 2);
    for byte in random {
        write!(&mut request_id, "{byte:02x}").expect("writing to a String cannot fail");
    }
    Ok(request_id)
}

/// How a 404 is read for one operation.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Kind {
    /// `/status`: always answers when the extension is built in, and its
    /// protocol version is checked.
    Status,
    /// A path that never answers 404 from the extension, so a 404 means the
    /// Core does not have the extension.
    Fixed,
    /// A 404 names an account, job or session that does not exist.
    Lookup,
}

/// A validated operation with its method, route, query, encoded body and
/// request id fixed. Each attempt to send it sends the same bytes.
pub struct PreparedRequest {
    kind: Kind,
    method: Method,
    route: String,
    query: Vec<(&'static str, String)>,
    body: Option<Vec<u8>>,
    request_id: Option<String>,
}
impl PreparedRequest {
    fn read(kind: Kind, route: String) -> Self {
        Self {
            kind,
            method: Method::GET,
            route,
            query: Vec::new(),
            body: None,
            request_id: None,
        }
    }

    fn write<T: Serialize>(
        kind: Kind,
        method: Method,
        route: String,
        request_id: String,
        body: &T,
    ) -> Result<Self, String> {
        // Encoded once here; the transport retry resends these exact bytes.
        let body = serde_json::to_vec(body)
            .map_err(|_| "unable to encode the check-in request".to_string())?;
        Ok(Self {
            kind,
            method,
            route,
            query: Vec::new(),
            body: Some(body),
            request_id: Some(request_id),
        })
    }

    fn with_query(mut self, query: Vec<(&'static str, String)>) -> Self {
        self.query = query;
        self
    }

    pub fn method(&self) -> &Method {
        &self.method
    }

    /// The route below [`API_PREFIX`], without the query.
    pub fn route(&self) -> &str {
        &self.route
    }

    /// Query pairs, not yet encoded.
    pub fn query(&self) -> &[(&'static str, String)] {
        &self.query
    }

    pub fn body(&self) -> Option<&[u8]> {
        self.body.as_deref()
    }
    /// Classifies Core's answer. A 2xx means Core accepted or answered the
    /// exchange; whether a check-in happened is only ever read from the job
    /// receipt's own `status`, never from the HTTP status.
    pub fn answer(&self, status: u16, body: &[u8]) -> OperationResult {
        if status == 404 && self.kind != Kind::Lookup {
            // A Core without the extension answers its generic 404, whose
            // wording says nothing useful to the window.
            return OperationResult {
                outcome: Outcome::ExtensionUnavailable,
                http_status: Some(status),
                request_id: self.request_id.clone(),
                body: serde_json::Value::Null,
                error_code: None,
                retryable: false,
            };
        }
        let parsed = if body.iter().all(u8::is_ascii_whitespace) {
            Some(serde_json::Value::Null)
        } else {
            serde_json::from_slice(body).ok()
        };
        let outcome = match status {
            200..=299 if parsed.is_none() => Outcome::Unexpected,
            200..=299 if self.kind == Kind::Status && !speaks_protocol(parsed.as_ref()) => {
                Outcome::ExtensionUnsupported
            }
            200..=299 => Outcome::Accepted,
            400 | 405 | 413 | 415 => Outcome::Rejected,
            401 | 403 => Outcome::Forbidden,
            404 => Outcome::NotFound,
            409 => Outcome::Conflict,
            412 => Outcome::RevisionConflict,
            429 | 503 => Outcome::TemporarilyUnavailable,
            500..=599 => Outcome::Failed,
            _ => Outcome::Unexpected,
        };
        let (error_code, retryable) = match (&parsed, status) {
            (Some(body), 400..=599) => error_summary(body),
            _ => (None, false),
        };
        OperationResult {
            outcome,
            http_status: Some(status),
            request_id: self.request_id.clone(),
            body: parsed.unwrap_or(serde_json::Value::Null),
            error_code,
            retryable,
        }
    }
    /// No usable answer arrived. A write keeps its `request_id`, so resending
    /// the same action replays Core's receipt rather than acting twice.
    pub fn unanswered(&self, outcome: Outcome) -> OperationResult {
        OperationResult {
            outcome,
            http_status: None,
            request_id: self.request_id.clone(),
            body: serde_json::Value::Null,
            error_code: None,
            retryable: true,
        }
    }
}

// Bodies may hold operator-entered addresses; only their size is printed.
impl std::fmt::Debug for PreparedRequest {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("PreparedRequest")
            .field("method", &self.method)
            .field("route", &self.route)
            .field("request_id", &self.request_id)
            .field("body_bytes", &self.body.as_ref().map(Vec::len))
            .finish_non_exhaustive()
    }
}

fn speaks_protocol(body: Option<&serde_json::Value>) -> bool {
    body.and_then(|body| body.get("protocol_version"))
        .and_then(serde_json::Value::as_u64)
        == Some(PROTOCOL_VERSION)
}

// Only a code shaped like the contract's is passed on; Core's messages are
// already sanitized but the window localizes from the code.
fn error_summary(body: &serde_json::Value) -> (Option<String>, bool) {
    let Some(error) = body.get("error") else {
        return (None, false);
    };
    let code = error
        .get("code")
        .and_then(serde_json::Value::as_str)
        .filter(|code| is_error_code(code))
        .map(str::to_string);
    let retryable = error
        .get("retryable")
        .and_then(serde_json::Value::as_bool)
        .unwrap_or(false);
    (code, retryable)
}

fn is_error_code(value: &str) -> bool {
    let bytes = value.as_bytes();
    (1..=96).contains(&bytes.len())
        && bytes[0].is_ascii_lowercase()
        && bytes
            .iter()
            .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || *byte == b'_')
}
/// What the window receives for every operation that was sent or attempted.
#[derive(Debug, Serialize)]
pub struct OperationResult {
    pub outcome: Outcome,
    pub http_status: Option<u16>,
    /// The idempotency key of a write. Resending the same logical action
    /// with this id is always safe. Absent for reads.
    pub request_id: Option<String>,
    /// Core's parsed JSON answer, or null.
    pub body: serde_json::Value,
    pub error_code: Option<String>,
    pub retryable: bool,
}

/// The HTTP exchange, not the check-in: a job's own `status` says whether a
/// site was checked in.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Outcome {
    /// Core answered 2xx. For a job this means the receipt was stored or
    /// read, not that the job finished.
    Accepted,
    /// The Core has no check-in extension; only this feature is unavailable.
    ExtensionUnavailable,
    /// The extension speaks another protocol version.
    ExtensionUnsupported,
    Rejected,
    /// The control token was refused; the host never holds a lesser one.
    Forbidden,
    NotFound,
    /// `request_id_reused`, `checkin_disabled`, `account_busy`, ...
    Conflict,
    RevisionConflict,
    TemporarilyUnavailable,
    Failed,
    Unexpected,
    /// No answer arrived. The write may or may not have been applied; resend
    /// it with the same `request_id`.
    TransportFailed,
    /// Core is not running or not ready; nothing was sent.
    CoreNotReady,
}

#[cfg(test)]
mod tests {
    use super::*;
    use base64::Engine as _;
    use serde_json::{json, Value};

    fn parse(value: Value) -> Result<PreparedRequest, String> {
        let operation: CheckinOperation =
            serde_json::from_value(value).map_err(|error| error.to_string())?;
        operation.prepare()
    }

    /// The desktop window builds the payload itself, so its exact shape is
    /// part of this module's contract: the two forms `completeCheckin\
    /// Authorization` sends must deserialize and prepare unchanged.
    #[test]
    fn the_payload_the_paste_window_sends_is_accepted_as_it_is() {
        // First attempt: no request_id yet, no claimed_user_id offered.
        let first = parse(json!({
            "op": "complete_authorization",
            "session_id": "session_0001",
            "pasted_cookies": "session=one; csrf=two",
            "dashboard_base_url": "https://relay.example",
            "expected_revision": 2,
        }))
        .expect("the window's first submit must be accepted");
        assert_eq!(first.route(), "/authorizations/session_0001/complete");
        // Core issued an id on a lost answer; the retry replays it.
        let retry = parse(json!({
            "op": "complete_authorization",
            "session_id": "session_0001",
            "pasted_cookies": "session=one; csrf=two",
            "dashboard_base_url": "https://relay.example",
            "expected_revision": 2,
            "request_id": "req_native_one",
        }))
        .expect("the window's retry must be accepted");
        let id = |prepared: &PreparedRequest| {
            body_json(prepared)["request_id"]
                .as_str()
                .expect("a completion always carries its request id")
                .to_owned()
        };
        assert_eq!(id(&retry), "req_native_one");
        // An absent id is generated here, not borrowed from another request.
        assert_ne!(id(&first), id(&retry));
        for prepared in [&first, &retry] {
            let body = body_json(prepared);
            let mut keys: Vec<&str> = body
                .as_object()
                .expect("the completion body is an object")
                .keys()
                .map(String::as_str)
                .collect();
            keys.sort_unstable();
            assert_eq!(keys, ["credential", "expected_revision", "request_id"]);
        }
    }

    fn body_json(prepared: &PreparedRequest) -> Value {
        prepared
            .body()
            .map(|body| serde_json::from_slice(body).unwrap())
            .unwrap_or(Value::Null)
    }

    /// Operation, then the method, route, query pairs and body it must send.
    type RouteCase = (
        Value,
        Method,
        &'static str,
        Vec<(&'static str, &'static str)>,
        Value,
    );

    #[test]
    fn custom_network_reuses_public_proxy_validation() {
        for address in [
            "http://127.0.0.1:7890",
            "https://proxy.example:443",
            "socks5://127.0.0.1:1080",
            "socks5://[::1]:1080",
        ] {
            for operation in [
                json!({"op": "create_account", "dashboard_base_url": "https://relay.example", "time_zone": "UTC"}),
                json!({"op": "update_account", "account_id": "acct_one", "expected_revision": 1}),
            ] {
                let mut operation = operation;
                operation["network"] = json!({"mode": "custom", "proxy_url": address});
                let prepared = parse(operation).unwrap();
                assert_eq!(body_json(&prepared)["network"]["proxy_url"], address);
            }
        }
        for address in [
            "socks5://user:password@proxy.example:1080",
            "socks5://proxy.example:0",
            "http://proxy.example/path",
            "socks5://proxy.example:1080?next=1",
            "socks5://proxy.example:1080#fragment",
            "file:///tmp/proxy",
        ] {
            let result = parse(
                json!({"op": "create_account", "dashboard_base_url": "https://relay.example",
                "time_zone": "UTC", "network": {"mode": "custom", "proxy_url": address}}),
            );
            let error = result.unwrap_err();
            assert!(!error.contains(address));
            assert!(!error.contains("password"));
        }
    }

    #[test]
    fn every_operation_maps_to_one_fixed_route() {
        let rid = "request_fixed_01";
        let cases: Vec<RouteCase> = vec![
            (
                json!({"op": "status"}),
                Method::GET,
                "/status",
                vec![],
                Value::Null,
            ),
            (
                json!({"op": "get_settings"}),
                Method::GET,
                "/settings",
                vec![],
                Value::Null,
            ),
            (
                json!({"op": "update_settings", "request_id": rid, "enabled": true}),
                Method::PUT,
                "/settings",
                vec![],
                json!({"request_id": rid, "enabled": true}),
            ),
            (
                json!({"op": "list_accounts"}),
                Method::GET,
                "/accounts",
                vec![],
                Value::Null,
            ),
            (
                json!({"op": "list_accounts", "limit": 20, "cursor": "a/b c"}),
                Method::GET,
                "/accounts",
                vec![("limit", "20"), ("cursor", "a/b c")],
                Value::Null,
            ),
            (
                json!({"op": "get_account", "account_id": "acct_one"}),
                Method::GET,
                "/accounts/acct_one",
                vec![],
                Value::Null,
            ),
            (
                json!({"op": "create_account", "request_id": rid,
                       "dashboard_base_url": "https://relay.example", "time_zone": "Asia/Shanghai",
                       "network": {"mode": "custom", "proxy_url": "http://127.0.0.1:7890"}}),
                Method::POST,
                "/accounts",
                vec![],
                json!({"request_id": rid, "dashboard_base_url": "https://relay.example",
                       "time_zone": "Asia/Shanghai",
                       "network": {"mode": "custom", "proxy_url": "http://127.0.0.1:7890"}}),
            ),
            (
                json!({"op": "update_account", "account_id": "acct_one", "request_id": rid,
                       "expected_revision": 3, "automatic": true, "bound_services": ["service_a"]}),
                Method::PATCH,
                "/accounts/acct_one",
                vec![],
                json!({"request_id": rid, "expected_revision": 3, "automatic": true,
                       "bound_services": ["service_a"]}),
            ),
            (
                json!({"op": "delete_account", "account_id": "acct_one", "request_id": rid,
                       "expected_revision": 4}),
                Method::DELETE,
                "/accounts/acct_one",
                vec![("request_id", rid), ("expected_revision", "4")],
                Value::Null,
            ),
            (
                json!({"op": "list_jobs", "limit": 5, "account_id": "acct_one"}),
                Method::GET,
                "/jobs",
                vec![("limit", "5"), ("account_id", "acct_one")],
                Value::Null,
            ),
            (
                json!({"op": "get_job", "job_id": "batch_0a1b"}),
                Method::GET,
                "/jobs/batch_0a1b",
                vec![],
                Value::Null,
            ),
            (
                json!({"op": "create_job", "request_id": rid, "action": "check_in",
                       "accounts": ["acct_one"], "expected_revision": 2}),
                Method::POST,
                "/jobs",
                vec![],
                json!({"request_id": rid, "action": "check_in", "accounts": ["acct_one"],
                       "expected_revision": 2}),
            ),
            (
                json!({"op": "cancel_job", "job_id": "job_0a1b", "request_id": rid}),
                Method::POST,
                "/jobs/job_0a1b/cancel",
                vec![],
                json!({"request_id": rid}),
            ),
            (
                json!({"op": "begin_authorization", "request_id": rid, "account_id": "acct_one",
                       "expected_revision": 1}),
                Method::POST,
                "/authorizations",
                vec![],
                json!({"request_id": rid, "account_id": "acct_one", "expected_revision": 1}),
            ),
        ];
        for (operation, method, route, query, body) in cases {
            let label = operation.to_string();
            let prepared = parse(operation).unwrap();
            assert_eq!(prepared.method(), &method, "{label}");
            assert_eq!(prepared.route(), route, "{label}");
            let actual: Vec<(&str, &str)> = prepared
                .query()
                .iter()
                .map(|(name, value)| (*name, value.as_str()))
                .collect();
            assert_eq!(actual, query, "{label}");
            assert_eq!(body_json(&prepared), body, "{label}");
        }
        // Field order is fixed, so equal content always encodes identically.
        let prepared =
            parse(json!({"op": "update_settings", "request_id": rid, "enabled": false})).unwrap();
        assert_eq!(
            prepared.body().unwrap(),
            br#"{"request_id":"request_fixed_01","enabled":false}"#
        );
    }
    #[test]
    fn callers_cannot_name_urls_methods_headers_or_unknown_operations() {
        let rejected = [
            json!({}),
            json!({"op": "fetch", "url": "https://relay.example/api/user/checkin"}),
            // `complete_authorization` exists now, so an operation name that
            // does not keeps the default-deny assertion honest.
            json!({"op": "capture_session", "session_id": "session_01",
                   "request_id": "request_c_01", "credential": "Y29va2ll"}),
            // The real operation still refuses a caller-named credential
            // envelope: only a raw paste is accepted, and the host converts it.
            json!({"op": "complete_authorization", "session_id": "session_0001",
                   "dashboard_base_url": "https://relay.example",
                   "credential": "Y29va2ll"}),
            json!({"op": "Status"}),
            json!({"op": "status", "url": "http://127.0.0.1:1/control/v1/services"}),
            json!({"op": "status", "method": "DELETE"}),
            json!({"op": "get_settings", "headers": {"Authorization": "Bearer x"}}),
            json!({"op": "get_account", "account_id": "acct_one", "path": "/../services"}),
            json!({"op": "list_accounts", "account_id": "acct_one"}),
            json!({"op": "update_settings", "enabled": true, "body": "{}"}),
            json!({"op": "create_job", "action": "check_in", "accounts": ["acct_one"],
                   "trigger": "scheduled"}),
            json!({"op": "create_job", "action": "submit", "accounts": ["acct_one"]}),
            json!({"op": "create_account", "dashboard_base_url": "https://relay.example",
                   "time_zone": "UTC", "network": {"mode": "direct", "username": "u"}}),
            json!({"op": "update_account", "account_id": "acct_one", "expected_revision": 1,
                   "remote_user_id": "7"}),
            json!({"op": "cancel_job", "job_id": "job_one", "query": "force=1"}),
        ];
        for value in rejected {
            assert!(parse(value.clone()).is_err(), "{value}");
        }
    }

    #[test]
    fn identifiers_follow_the_contract_patterns() {
        let long = "a".repeat(65);
        for account in [
            "",
            "ab",
            "Acct_one",
            "1acct",
            "acct-one",
            "acct/one",
            "acct%2fone",
            "acct one",
            "acct.one",
            "..",
            "acct_one?x=1",
            long.as_str(),
        ] {
            let value = json!({"op": "get_account", "account_id": account});
            assert!(parse(value.clone()).is_err(), "{value}");
        }
        assert!(parse(json!({"op": "get_account", "account_id": "a".repeat(64)})).is_ok());
        for job in [
            "",
            "jo",
            "Job_one",
            "job-one",
            "job/one",
            "job_one/cancel",
            long.as_str(),
        ] {
            let value = json!({"op": "get_job", "job_id": job});
            assert!(parse(value.clone()).is_err(), "{value}");
            let value = json!({"op": "cancel_job", "job_id": job});
            assert!(parse(value.clone()).is_err(), "{value}");
        }
        assert!(parse(json!({"op": "get_job", "job_id": "0ab"})).is_ok());
        let long_request = "r".repeat(129);
        for request_id in [
            "",
            "short_1",
            "request id",
            "request/id",
            "request.id",
            long_request.as_str(),
        ] {
            let value = json!({"op": "update_settings", "request_id": request_id, "enabled": true});
            assert!(parse(value.clone()).is_err(), "{value}");
        }
        for services in [
            json!(["Service_a"]),
            json!(["-svc"]),
            json!(["sv"]),
            json!(["a/b"]),
        ] {
            let value = json!({"op": "update_account", "account_id": "acct_one",
                               "expected_revision": 1, "bound_services": services});
            assert!(parse(value.clone()).is_err(), "{value}");
        }
        let too_many: Vec<String> = (0..33).map(|index| format!("service_{index}")).collect();
        assert!(
            parse(json!({"op": "update_account", "account_id": "acct_one",
                             "expected_revision": 1, "bound_services": too_many}))
            .is_err()
        );
    }
    #[test]
    fn writes_get_one_request_id_per_action_and_resume_keeps_it() {
        let action =
            json!({"op": "create_job", "action": "status_refresh", "accounts": ["acct_one"]});
        let first = parse(action.clone()).unwrap();
        let second = parse(action.clone()).unwrap();
        let first_id = first.request_id.clone().unwrap();
        assert_eq!(first_id.len(), 32);
        assert!(is_request_id(&first_id));
        assert!(first_id.bytes().all(|byte| byte.is_ascii_hexdigit()));
        assert_ne!(
            Some(first_id.clone()),
            second.request_id,
            "two actions share a key"
        );
        assert_eq!(body_json(&first)["request_id"], json!(first_id));

        // Retrying the same logical action with the returned id resends the
        // same bytes, so Core replays its receipt.
        let mut resumed = action;
        resumed["request_id"] = json!(first_id);
        let resumed = parse(resumed).unwrap();
        assert_eq!(resumed.body(), first.body());
        assert_eq!(resumed.request_id, first.request_id);

        for read in [
            json!({"op": "status"}),
            json!({"op": "get_settings"}),
            json!({"op": "list_jobs"}),
            json!({"op": "get_job", "job_id": "job_one"}),
        ] {
            let prepared = parse(read).unwrap();
            assert!(prepared.request_id.is_none());
            assert!(prepared.body().is_none());
        }
        let delete = parse(json!({"op": "delete_account", "account_id": "acct_one",
                                  "expected_revision": 1}))
        .unwrap();
        assert!(delete.body().is_none(), "delete takes no body");
        let delete_id = delete.request_id.clone().unwrap();
        assert_eq!(delete.query()[0], ("request_id", delete_id));
    }

    #[test]
    fn job_requests_follow_the_batch_rules() {
        let accounts: Vec<String> = (0..26).map(|index| format!("acct_{index:02}")).collect();
        let job = |accounts: Value, revision: Value| {
            let mut value = json!({"op": "create_job", "action": "check_in", "accounts": accounts});
            if !revision.is_null() {
                value["expected_revision"] = revision;
            }
            parse(value)
        };
        assert!(job(json!(&accounts[..25]), Value::Null).is_ok());
        assert!(job(json!(&accounts[..2]), Value::Null).is_ok());
        assert!(job(json!(["acct_one"]), json!(7)).is_ok());
        for (accounts, revision) in [
            (json!([]), Value::Null),
            (json!(accounts), Value::Null),
            (json!(["acct_one", "acct_one"]), Value::Null),
            (json!(["acct_one", "acct_two"]), json!(1)),
            (json!(["acct_one"]), json!(0)),
            (json!(["acct_one"]), json!(-1)),
            (json!(["acct_one"]), json!(1.5)),
            (json!(["acct_one"]), json!("1")),
            (json!(["acct_one"]), json!(1_u64 << 53)),
            (json!("acct_one"), Value::Null),
        ] {
            assert!(
                job(accounts.clone(), revision.clone()).is_err(),
                "{accounts} {revision}"
            );
        }
    }
    #[test]
    fn account_writes_are_validated_before_sending() {
        let rejected = [
            json!({"op": "update_account", "account_id": "acct_one", "expected_revision": 1}),
            json!({"op": "update_account", "account_id": "acct_one", "expected_revision": 0,
                   "automatic": true}),
            json!({"op": "update_account", "account_id": "acct_one", "automatic": true}),
            json!({"op": "update_account", "account_id": "acct_one", "expected_revision": 1,
                   "time_zone": "Asia/Shang\nhai"}),
            json!({"op": "update_account", "account_id": "acct_one", "expected_revision": 1,
                   "network": {"mode": "custom"}}),
            json!({"op": "update_account", "account_id": "acct_one", "expected_revision": 1,
                   "network": {"mode": "direct", "proxy_url": "http://127.0.0.1:7890"}}),
            json!({"op": "update_account", "account_id": "acct_one", "expected_revision": 1,
                   "network": {"mode": "tunnel"}}),
            json!({"op": "create_account", "time_zone": "UTC"}),
            json!({"op": "create_account", "dashboard_base_url": "https://relay.example"}),
            json!({"op": "create_account", "dashboard_base_url": "ftp://relay.example",
                   "time_zone": "UTC"}),
            json!({"op": "create_account", "dashboard_base_url": "relay.example",
                   "time_zone": "UTC"}),
            json!({"op": "create_account", "dashboard_base_url": "", "time_zone": "UTC"}),
            json!({"op": "create_account", "dashboard_base_url": "https://relay.example",
                   "time_zone": ""}),
            json!({"op": "create_account",
                   "dashboard_base_url": format!("https://{}", "a".repeat(MAX_URL_BYTES)),
                   "time_zone": "UTC"}),
            json!({"op": "delete_account", "account_id": "acct_one"}),
            json!({"op": "begin_authorization", "account_id": "acct_one"}),
            json!({"op": "list_accounts", "limit": 0}),
            json!({"op": "list_accounts", "limit": 101}),
            json!({"op": "list_jobs", "cursor": ""}),
            json!({"op": "list_jobs", "cursor": "c".repeat(MAX_CURSOR_BYTES + 1)}),
            json!({"op": "list_jobs", "account_id": "Acct"}),
        ];
        for value in rejected {
            assert!(parse(value.clone()).is_err(), "{value}");
        }
        assert!(parse(
            json!({"op": "create_account", "dashboard_base_url": "HTTPS://relay.example/sub",
                             "time_zone": "UTC"})
        )
        .is_ok());
        assert!(
            parse(json!({"op": "update_account", "account_id": "acct_one",
                             "expected_revision": 1, "network": {"mode": "system"}}))
            .is_ok()
        );
    }
    fn envelope(code: &str, retryable: bool) -> Vec<u8> {
        serde_json::to_vec(&json!({
            "error": {"code": code, "message": "sanitized", "retryable": retryable, "details": []},
            "request_id": "req_core"
        }))
        .unwrap()
    }

    #[test]
    fn answers_classify_the_exchange_not_the_check_in() {
        let job = parse(json!({"op": "create_job", "request_id": "request_class_1",
                               "action": "check_in", "accounts": ["acct_one"]}))
        .unwrap();
        let receipt = br#"{"id":"job_0a","account_id":"acct_one","action":"check_in","status":"queued","dispatched":false,"proof_source":"none","created_at":"2025-01-01T00:00:00Z","children":[]}"#;
        let accepted = job.answer(202, receipt);
        assert_eq!(accepted.outcome, Outcome::Accepted);
        assert_eq!(
            accepted.body["status"],
            json!("queued"),
            "202 only queues a job"
        );
        assert_eq!(accepted.request_id.as_deref(), Some("request_class_1"));
        assert_eq!(accepted.http_status, Some(202));
        assert!(accepted.error_code.is_none());

        let cases = [
            (409, "request_id_reused", false, Outcome::Conflict),
            (409, "checkin_disabled", false, Outcome::Conflict),
            (409, "account_busy", true, Outcome::Conflict),
            (412, "revision_conflict", false, Outcome::RevisionConflict),
            (400, "validation_failed", false, Outcome::Rejected),
            (413, "payload_too_large", false, Outcome::Rejected),
            (415, "unsupported_media_type", false, Outcome::Rejected),
            (401, "unauthorized", false, Outcome::Forbidden),
            (403, "forbidden", false, Outcome::Forbidden),
            (404, "not_found", false, Outcome::NotFound),
            (
                503,
                "checkin_stopping",
                true,
                Outcome::TemporarilyUnavailable,
            ),
            (500, "checkin_storage_failed", false, Outcome::Failed),
        ];
        for (status, code, retryable, outcome) in cases {
            let result = job.answer(status, &envelope(code, retryable));
            assert_eq!(result.outcome, outcome, "{status} {code}");
            assert_eq!(result.error_code.as_deref(), Some(code), "{status} {code}");
            assert_eq!(result.retryable, retryable, "{status} {code}");
            assert_eq!(result.request_id.as_deref(), Some("request_class_1"));
        }
        let odd = job.answer(409, &envelope("Not A Code", true));
        assert!(odd.error_code.is_none());
        assert_eq!(job.answer(200, b"<html>").outcome, Outcome::Unexpected);
        assert_eq!(job.answer(302, b"").outcome, Outcome::Unexpected);
        let broken = job.answer(500, b"not json");
        assert_eq!(
            (broken.outcome, broken.body.clone()),
            (Outcome::Failed, Value::Null)
        );
        let deleted = parse(json!({"op": "delete_account", "account_id": "acct_one",
                                   "request_id": "request_del_01", "expected_revision": 2}))
        .unwrap()
        .answer(204, b"");
        assert_eq!(
            (deleted.outcome, deleted.body),
            (Outcome::Accepted, Value::Null)
        );
    }
    #[test]
    fn status_negotiates_the_protocol_version() {
        let status = parse(json!({"op": "status"})).unwrap();
        let current = br#"{"protocol_version":1,"present":true,"enabled":false,"storage_ready":false,"scheduler_running":false}"#;
        assert_eq!(status.answer(200, current).outcome, Outcome::Accepted);
        let newer = br#"{"protocol_version":2,"present":true,"enabled":true,"storage_ready":true,"scheduler_running":true}"#;
        assert_eq!(
            status.answer(200, newer).outcome,
            Outcome::ExtensionUnsupported
        );
        assert_eq!(
            status.answer(200, b"{}").outcome,
            Outcome::ExtensionUnsupported
        );
        assert_eq!(
            status.answer(200, b"").outcome,
            Outcome::ExtensionUnsupported
        );
    }

    #[test]
    fn a_404_means_extension_unavailable_only_where_the_extension_never_answers_404() {
        let generic = envelope("not_found", false);
        for operation in [
            json!({"op": "status"}),
            json!({"op": "get_settings"}),
            json!({"op": "update_settings", "enabled": true}),
            json!({"op": "list_accounts"}),
            json!({"op": "create_account", "dashboard_base_url": "https://relay.example",
                   "time_zone": "UTC"}),
            json!({"op": "delete_account", "account_id": "acct_one", "expected_revision": 1}),
            json!({"op": "list_jobs"}),
        ] {
            let result = parse(operation.clone()).unwrap().answer(404, &generic);
            assert_eq!(result.outcome, Outcome::ExtensionUnavailable, "{operation}");
            assert_eq!(result.body, Value::Null, "{operation}");
            assert!(
                result.error_code.is_none() && !result.retryable,
                "{operation}"
            );
        }
        for operation in [
            json!({"op": "get_account", "account_id": "acct_missing"}),
            json!({"op": "update_account", "account_id": "acct_missing", "expected_revision": 1,
                   "automatic": false}),
            json!({"op": "get_job", "job_id": "job_missing"}),
            json!({"op": "create_job", "action": "check_in", "accounts": ["acct_missing"]}),
            json!({"op": "cancel_job", "job_id": "job_missing"}),
            json!({"op": "begin_authorization", "account_id": "acct_missing",
                   "expected_revision": 1}),
        ] {
            let result = parse(operation.clone()).unwrap().answer(404, &generic);
            assert_eq!(result.outcome, Outcome::NotFound, "{operation}");
            assert_eq!(
                result.error_code.as_deref(),
                Some("not_found"),
                "{operation}"
            );
        }
    }

    #[test]
    fn debug_and_results_never_carry_request_bodies() {
        let prepared = parse(json!({"op": "create_account", "request_id": "request_dbg_01",
                                    "dashboard_base_url": "https://relay.example/private-sub",
                                    "time_zone": "UTC",
                                    "network": {"mode": "custom", "proxy_url": "http://10.0.0.9:7890"}}))
        .unwrap();
        let debug = format!("{prepared:?}");
        assert!(
            !debug.contains("private-sub") && !debug.contains("10.0.0.9"),
            "{debug}"
        );
        assert!(
            debug.contains("request_dbg_01") && debug.contains("body_bytes"),
            "{debug}"
        );
        let lost = prepared.unanswered(Outcome::TransportFailed);
        let shape = serde_json::to_value(&lost).unwrap();
        assert_eq!(
            shape,
            json!({"outcome": "transport_failed", "http_status": null,
                   "request_id": "request_dbg_01", "body": null, "error_code": null,
                   "retryable": true})
        );
    }

    #[test]
    fn wire_names_stay_what_the_desktop_bridge_accepts() {
        // apps/desktop/src/features/fork-checkin/bridge.ts accepts exactly these
        // outcome names and result keys and treats anything else as malformed,
        // so renaming one here must change the bridge with it.
        let names = [
            (Outcome::Accepted, "accepted"),
            (Outcome::ExtensionUnavailable, "extension_unavailable"),
            (Outcome::ExtensionUnsupported, "extension_unsupported"),
            (Outcome::Rejected, "rejected"),
            (Outcome::Forbidden, "forbidden"),
            (Outcome::NotFound, "not_found"),
            (Outcome::Conflict, "conflict"),
            (Outcome::RevisionConflict, "revision_conflict"),
            (Outcome::TemporarilyUnavailable, "temporarily_unavailable"),
            (Outcome::Failed, "failed"),
            (Outcome::Unexpected, "unexpected"),
            (Outcome::TransportFailed, "transport_failed"),
            (Outcome::CoreNotReady, "core_not_ready"),
        ];
        for (outcome, name) in names {
            assert_eq!(serde_json::to_value(outcome).unwrap(), json!(name));
        }

        let job = parse(json!({"op": "create_job", "request_id": "request_wire_1",
                               "action": "check_in", "accounts": ["acct_one"]}))
        .unwrap();
        let busy = serde_json::to_value(job.answer(409, &envelope("account_busy", true))).unwrap();
        assert_eq!(busy["outcome"], json!("conflict"));
        assert_eq!(busy["http_status"], json!(409));
        assert_eq!(busy["request_id"], json!("request_wire_1"));
        assert_eq!(busy["error_code"], json!("account_busy"));
        assert_eq!(busy["retryable"], json!(true));
        let mut keys: Vec<&str> = busy
            .as_object()
            .unwrap()
            .keys()
            .map(String::as_str)
            .collect();
        keys.sort_unstable();
        assert_eq!(
            keys,
            [
                "body",
                "error_code",
                "http_status",
                "outcome",
                "request_id",
                "retryable"
            ]
        );
    }

    #[test]
    fn completing_an_authorization_posts_the_converted_session() {
        let prepared = parse(json!({
            "op": "complete_authorization",
            "session_id": "session_0001",
            "request_id": "request_c_01",
            "pasted_cookies": "session=abc; csrf=def",
            "dashboard_base_url": "https://relay.example",
            "expected_revision": 3,
            "claimed_user_id": "7",
        }))
        .unwrap();
        assert_eq!(prepared.method(), &Method::POST);
        assert_eq!(prepared.route(), "/authorizations/session_0001/complete");
        assert!(prepared.query().is_empty());
        let body = body_json(&prepared);
        let mut keys: Vec<&str> = body
            .as_object()
            .unwrap()
            .keys()
            .map(String::as_str)
            .collect();
        keys.sort_unstable();
        assert_eq!(
            keys,
            [
                "claimed_user_id",
                "credential",
                "expected_revision",
                "request_id"
            ]
        );
        assert_eq!(body["request_id"], json!("request_c_01"));
        assert_eq!(body["expected_revision"], json!(3));
        // Passed through untouched; Core asks the site and stores its answer.
        assert_eq!(body["claimed_user_id"], json!("7"));
        // The paste is converted here, so the window never names an envelope.
        let envelope: Value = serde_json::from_slice(
            &base64::engine::general_purpose::STANDARD
                .decode(body["credential"].as_str().unwrap())
                .unwrap(),
        )
        .unwrap();
        assert_eq!(envelope["version"], json!(1));
        assert_eq!(envelope["cookies"].as_array().unwrap().len(), 2);
        assert_eq!(envelope["cookies"][0]["secure"], json!(true));
    }

    #[test]
    fn the_optional_completion_fields_are_omitted_when_absent() {
        let prepared = parse(json!({
            "op": "complete_authorization",
            "session_id": "session_0001",
            "request_id": "request_c_01",
            "pasted_cookies": "session=abc",
            "dashboard_base_url": "http://127.0.0.1:3000",
        }))
        .unwrap();
        // Asserted structurally: a session-shaped literal must not be
        // baked into the repository, and the key order is checked below.
        let raw = std::str::from_utf8(prepared.body().unwrap()).unwrap();
        assert!(
            raw.starts_with(r#"{"request_id":"request_c_01","credential":""#),
            "{raw}"
        );
        assert!(raw.ends_with(r#""}"#), "{raw}");
        assert!(!raw.contains("expected_revision"), "{raw}");
        assert!(!raw.contains("claimed_user_id"), "{raw}");
        // A plain-http account must not have its cookies marked Secure, or
        // the site would never receive them.
        let body = body_json(&prepared);
        let envelope: Value = serde_json::from_slice(
            &base64::engine::general_purpose::STANDARD
                .decode(body["credential"].as_str().unwrap())
                .unwrap(),
        )
        .unwrap();
        assert_eq!(envelope["cookies"][0]["secure"], json!(false));
    }

    #[test]
    fn a_completion_validates_every_identifier_before_building_the_route() {
        let base = json!({
            "op": "complete_authorization",
            "session_id": "session_0001",
            "request_id": "request_c_01",
            "pasted_cookies": "session=abc",
            "dashboard_base_url": "https://relay.example",
        });
        assert!(parse(base.clone()).is_ok());
        for (field, value) in [
            // A traversal attempt or a short id must never reach the route.
            ("session_id", json!("../../services")),
            ("session_id", json!("short")),
            ("session_id", json!("session 0001")),
            ("session_id", json!("a".repeat(129))),
            ("request_id", json!("short")),
            ("dashboard_base_url", json!("file:///etc/passwd")),
            ("dashboard_base_url", json!("relay.example")),
            ("expected_revision", json!(0)),
            ("claimed_user_id", json!("")),
            ("claimed_user_id", json!("a".repeat(129))),
            ("claimed_user_id", json!("seven\nuser")),
            // A paste the host cannot convert is refused before sending.
            ("pasted_cookies", json!("")),
            ("pasted_cookies", json!("sessionabc")),
            ("pasted_cookies", json!("session=a b")),
        ] {
            let mut candidate = base.clone();
            candidate[field] = value.clone();
            assert!(parse(candidate).is_err(), "{field}={value}");
        }
    }

    #[test]
    fn a_completion_never_echoes_the_session_anywhere() {
        let secret = "supersecretsessionvalue";
        let prepared = parse(json!({
            "op": "complete_authorization",
            "session_id": "session_0001",
            "request_id": "request_c_01",
            "pasted_cookies": format!("session={secret}"),
            "dashboard_base_url": "https://relay.example",
        }))
        .unwrap();
        // Debug prints only the size, so a log line cannot leak the session.
        let rendered = format!("{prepared:?}");
        assert!(!rendered.contains(secret), "{rendered}");
        assert!(rendered.contains("body_bytes"), "{rendered}");
        // Nor does any answer Core could give, including its own echo.
        for (status, body) in [
            (
                201_u16,
                br#"{"id":"acct_one","remote_user_id":"7"}"#.to_vec(),
            ),
            (409, br#"{"error":{"code":"identity_mismatch"}}"#.to_vec()),
            (
                500,
                format!(r#"{{"error":{{"code":"internal"}},"seen":"{secret}"}}"#).into_bytes(),
            ),
        ] {
            let result = prepared.answer(status, &body);
            let serialized = serde_json::to_string(&result).unwrap();
            if status == 500 {
                // Core's own body is returned as-is; what matters is that the
                // host added no copy of the credential of its own.
                assert_eq!(serialized.matches(secret).count(), 1, "{serialized}");
            } else {
                assert!(!serialized.contains(secret), "{serialized}");
            }
        }
        assert!(!format!("{:?}", prepared.unanswered(Outcome::TransportFailed)).contains(secret));
    }

    #[test]
    fn completion_failures_keep_the_request_id_and_cores_retryability() {
        let prepared = parse(json!({
            "op": "complete_authorization",
            "session_id": "session_0001",
            "request_id": "request_c_01",
            "pasted_cookies": "session=abc",
            "dashboard_base_url": "https://relay.example",
        }))
        .unwrap();
        // Retryability comes from Core's own flag, never from a local guess.
        for (status, code, outcome, retryable) in [
            (400_u16, "validation_failed", "rejected", false),
            (404, "not_found", "not_found", false),
            (409, "request_id_reused", "conflict", false),
            (409, "authorization_completed", "conflict", false),
            (409, "identity_mismatch", "conflict", false),
            (409, "auth_required", "conflict", false),
            (409, "manual_required", "conflict", false),
            (409, "unsupported", "conflict", false),
            (409, "account_busy", "conflict", true),
            (412, "revision_conflict", "revision_conflict", false),
            (413, "too_large", "rejected", false),
            (503, "site_unavailable", "temporarily_unavailable", true),
            (503, "rate_limited", "temporarily_unavailable", true),
        ] {
            let body =
                format!(r#"{{"error":{{"code":"{code}","retryable":{retryable}}}}}"#).into_bytes();
            let result = serde_json::to_value(prepared.answer(status, &body)).unwrap();
            assert_eq!(result["outcome"], json!(outcome), "{code}");
            assert_eq!(result["error_code"], json!(code), "{code}");
            assert_eq!(result["retryable"], json!(retryable), "{code}");
            // The same id is replayed, so a retry reads Core's receipt.
            assert_eq!(result["request_id"], json!("request_c_01"), "{code}");
        }
        // A 404 on this route means the session is gone, not that the
        // extension is missing: the route is a Lookup.
        let gone = serde_json::to_value(prepared.answer(404, b"{}")).unwrap();
        assert_eq!(gone["outcome"], json!("not_found"));
        // A lost answer keeps the id retryable without a second action.
        let lost = serde_json::to_value(prepared.unanswered(Outcome::TransportFailed)).unwrap();
        assert_eq!(lost["request_id"], json!("request_c_01"));
        assert_eq!(lost["retryable"], json!(true));
    }
}
