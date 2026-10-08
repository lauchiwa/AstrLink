//! Admission probe for the aggregated check-in login window (task C01).
//!
//! This module decides *whether* a login webview may be opened and which
//! isolated profile it must use. It deliberately contains no webview calls:
//! Tauri's `MockRuntime` has no real webview, so `cookies_for_url`,
//! `incognito` and profile teardown cannot be observed from a unit test.
//! Those remain real-device checks recorded in [`REAL_DEVICE_CHECKS`].
//!
//! Nothing here reads or stores credentials. A failed decision must disable
//! native session capture for that platform rather than relax the main
//! window's permissions or the global CSP.

use std::collections::BTreeSet;

/// The fixed dependency used by this admission probe. It is reported with the
/// platform evidence so a real-device result cannot be reused after silently
/// changing the WebView implementation.
pub const TAURI_VERSION: &str = "2.11.5";

/// Build/runtime facts recorded alongside the real-device checklist. The
/// checklist remains unverified until a device exercises an actual WebView.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AdmissionEvidence {
    pub platform: &'static str,
    pub architecture: &'static str,
    pub tauri_version: &'static str,
    pub app_version: &'static str,
    pub unverified_checks: &'static [&'static str],
}

pub const fn admission_evidence() -> AdmissionEvidence {
    AdmissionEvidence {
        platform: std::env::consts::OS,
        architecture: std::env::consts::ARCH,
        tauri_version: TAURI_VERSION,
        app_version: env!("CARGO_PKG_VERSION"),
        unverified_checks: REAL_DEVICE_CHECKS,
    }
}

/// Windows that may ask the host to open a login webview. Any other source,
/// including a remote page loaded inside a login window itself, is refused.
pub const MAIN_WINDOW_LABEL: &str = "main";
const LOGIN_LABEL_PREFIX: &str = "fork-checkin-login-";
/// One login window per account, and a small ceiling so a scripted page
/// cannot exhaust webviews.
pub const MAX_CONCURRENT_LOGIN_WINDOWS: usize = 3;

/// Why a login window was refused. The caller maps this to a user-visible
/// reason; it never carries site content.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RefusalReason {
    /// The request arrived from a window other than the main app window.
    UntrustedSource,
    /// The platform has no recorded real-device evidence for capture.
    PlatformNotAdmitted,
    /// The URL is not an absolute `https` dashboard URL.
    UnsupportedUrl,
    /// The account identifier cannot form a stable profile.
    InvalidAccount,
    /// Too many login windows are already open.
    TooManyWindows,
}

/// A platform is admitted only after C01/G02 evidence is recorded for it.
/// `false` means manual check-in, never silent fallback to shared cookies.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct PlatformAdmission {
    pub isolated_profiles: bool,
    pub http_only_cookie_read: bool,
}

impl PlatformAdmission {
    pub const fn denied() -> Self {
        Self {
            isolated_profiles: false,
            http_only_cookie_read: false,
        }
    }

    /// Capture needs both an isolated profile and HTTP-only cookie reads;
    /// one without the other would either mix accounts or silently miss the
    /// session cookie.
    pub fn allows_capture(self) -> bool {
        self.isolated_profiles && self.http_only_cookie_read
    }
}

/// An approved login window: an isolated profile plus the origin its
/// navigation is confined to.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LoginWindowPlan {
    pub label: String,
    pub profile_identifier: String,
    pub incognito: bool,
    pub origin: String,
}

/// Decides whether `account_id` may open a login window for `dashboard_url`.
pub fn plan_login_window(
    source_window_label: &str,
    admission: PlatformAdmission,
    account_id: &str,
    dashboard_url: &str,
    open_labels: &BTreeSet<String>,
) -> Result<LoginWindowPlan, RefusalReason> {
    // Checked before anything else: a remote page must not learn whether an
    // account or URL is valid.
    if source_window_label != MAIN_WINDOW_LABEL {
        return Err(RefusalReason::UntrustedSource);
    }
    if !admission.allows_capture() {
        return Err(RefusalReason::PlatformNotAdmitted);
    }
    if !is_supported_account_id(account_id) {
        return Err(RefusalReason::InvalidAccount);
    }
    let origin = https_origin(dashboard_url).ok_or(RefusalReason::UnsupportedUrl)?;
    let label = login_label(account_id);
    // Reopening the same account reuses its window instead of counting again.
    if !open_labels.contains(&label) && open_labels.len() >= MAX_CONCURRENT_LOGIN_WINDOWS {
        return Err(RefusalReason::TooManyWindows);
    }
    Ok(LoginWindowPlan {
        profile_identifier: profile_identifier(account_id),
        label,
        // Never reuses the main window's session, so signing in to one
        // account cannot sign the operator out of another.
        incognito: true,
        origin,
    })
}

fn is_supported_account_id(account_id: &str) -> bool {
    // Match Core's AccountID contract before using it in a profile or label.
    (3..=64).contains(&account_id.len())
        && account_id.as_bytes()[0].is_ascii_lowercase()
        && account_id.bytes().all(|character| {
            character.is_ascii_lowercase() || character.is_ascii_digit() || character == b'_'
        })
}

/// Two accounts on the same site must never share a profile, so the account
/// identifier alone decides it.
pub fn profile_identifier(account_id: &str) -> String {
    format!("fork-checkin-{account_id}")
}

fn login_label(account_id: &str) -> String {
    format!("{LOGIN_LABEL_PREFIX}{account_id}")
}

/// Whether `label` belongs to a check-in login window.
pub fn is_login_label(label: &str) -> bool {
    label.starts_with(LOGIN_LABEL_PREFIX)
}

/// The origin of an absolute `https` URL, without its path, query or
/// fragment. Returns `None` for anything else, including `http`, so a login
/// window cannot be pointed at a local scheme or a plaintext page.
fn https_origin(url: &str) -> Option<String> {
    // WHATWG parsers may repair backslashes or strip control characters.
    // Refuse those inputs rather than disagree with the browser about origin.
    if url.len() > 2048
        || url
            .chars()
            .any(|c| c.is_whitespace() || c.is_control() || c == '\\')
    {
        return None;
    }
    let authority = url.split_once("://")?.1.split(['/', '?', '#']).next()?;
    if authority.is_empty() || authority.contains('@') {
        return None;
    }
    let parsed = reqwest::Url::parse(url).ok()?;
    if parsed.scheme() != "https"
        || !parsed.has_host()
        || !parsed.username().is_empty()
        || parsed.password().is_some()
        || parsed.port() == Some(0)
    {
        return None;
    }
    Some(parsed.origin().ascii_serialization())
}

/// What a login window may do with a navigation. OAuth has to leave the
/// dashboard origin, so navigation is allowed while capture stays pinned to
/// the account's own origin.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum NavigationDecision {
    /// Same origin as the account: navigation and capture are allowed.
    AllowAndCapture,
    /// A different https origin, such as an identity provider.
    AllowWithoutCapture,
    /// Local schemes, files and downloads.
    Block,
}

pub fn classify_navigation(plan: &LoginWindowPlan, url: &str) -> NavigationDecision {
    match https_origin(url) {
        Some(origin) if origin == plan.origin => NavigationDecision::AllowAndCapture,
        Some(_) => NavigationDecision::AllowWithoutCapture,
        None => NavigationDecision::Block,
    }
}

/// Behaviour that only a real device can show. Recorded so a passing unit
/// test is not mistaken for platform evidence.
pub const REAL_DEVICE_CHECKS: &[&str] = &[
    "incognito login window keeps its own cookie jar",
    "cookies_for_url returns HTTP-only session cookies",
    "two same-origin accounts do not share a session",
    "closing a login window clears only its own profile",
    "the main window's cookies and cache survive a login window closing",
    "Windows cookie reads run off the main thread without deadlocking",
    "remote pages cannot invoke privileged or check-in commands",
];

#[cfg(test)]
mod tests {
    use super::*;

    fn admitted() -> PlatformAdmission {
        PlatformAdmission {
            isolated_profiles: true,
            http_only_cookie_read: true,
        }
    }

    fn plan(account_id: &str, open: &[&str]) -> Result<LoginWindowPlan, RefusalReason> {
        let open_labels = open.iter().map(|label| (*label).to_string()).collect();
        plan_login_window(
            MAIN_WINDOW_LABEL,
            admitted(),
            account_id,
            "https://relay.example/dashboard?tab=1",
            &open_labels,
        )
    }

    #[test]
    fn same_site_accounts_get_separate_profiles() {
        let first = plan("account_one", &[]).unwrap();
        let second = plan("account_two", &[]).unwrap();
        assert_ne!(first.profile_identifier, second.profile_identifier);
        assert_ne!(first.label, second.label);
        assert_eq!(first.origin, second.origin);
        assert!(first.incognito && second.incognito);
        assert!(is_login_label(&first.label));
        assert!(!is_login_label(MAIN_WINDOW_LABEL));
    }

    #[test]
    fn a_window_other_than_main_cannot_open_a_login_window() {
        let open = BTreeSet::new();
        for source in [
            "fork-checkin-login-account_one",
            "trajectory-inspector-1",
            "raw-access-approval",
            "",
        ] {
            assert_eq!(
                plan_login_window(
                    source,
                    admitted(),
                    "account_one",
                    "https://relay.example/",
                    &open
                ),
                Err(RefusalReason::UntrustedSource),
                "source {source:?} was allowed to open a login window",
            );
        }
    }

    #[test]
    fn an_unadmitted_platform_refuses_before_looking_at_the_request() {
        let open = BTreeSet::new();
        for admission in [
            PlatformAdmission::denied(),
            PlatformAdmission {
                isolated_profiles: true,
                http_only_cookie_read: false,
            },
            PlatformAdmission {
                isolated_profiles: false,
                http_only_cookie_read: true,
            },
        ] {
            assert!(!admission.allows_capture());
            assert_eq!(
                plan_login_window(
                    MAIN_WINDOW_LABEL,
                    admission,
                    "account_one",
                    "https://relay.example/",
                    &open
                ),
                Err(RefusalReason::PlatformNotAdmitted),
            );
        }
    }

    #[test]
    fn only_absolute_https_dashboards_are_accepted() {
        for url in [
            "http://relay.example/",
            "file:///etc/passwd",
            "tauri://localhost",
            "javascript:alert(1)",
            "https://",
            "relay.example",
            "https://user:pass@relay.example/",
        ] {
            let open = BTreeSet::new();
            assert_eq!(
                plan_login_window(MAIN_WINDOW_LABEL, admitted(), "account_one", url, &open),
                Err(RefusalReason::UnsupportedUrl),
                "url {url} was accepted",
            );
        }
    }

    #[test]
    fn invalid_account_identifiers_are_refused() {
        for account in [
            "",
            "a",
            "ab",
            "1account",
            "Account_one",
            "account one",
            "account/../other",
            &"a".repeat(65),
        ] {
            assert_eq!(plan(account, &[]), Err(RefusalReason::InvalidAccount));
        }
    }

    #[test]
    fn concurrent_windows_are_bounded_but_reopening_one_account_is_not() {
        let open: Vec<String> = (0..MAX_CONCURRENT_LOGIN_WINDOWS)
            .map(|index| login_label(&format!("account_{index}")))
            .collect();
        let borrowed: Vec<&str> = open.iter().map(String::as_str).collect();
        assert_eq!(
            plan("account_new", &borrowed),
            Err(RefusalReason::TooManyWindows)
        );
        let reopened = plan("account_0", &borrowed).unwrap();
        assert_eq!(reopened.label, login_label("account_0"));
    }

    #[test]
    fn capture_is_pinned_to_the_account_origin_while_oauth_may_navigate() {
        let plan = plan("account_one", &[]).unwrap();
        assert_eq!(
            classify_navigation(&plan, "https://relay.example/login/step2"),
            NavigationDecision::AllowAndCapture,
        );
        assert_eq!(
            classify_navigation(&plan, "https://RELAY.EXAMPLE/login"),
            NavigationDecision::AllowAndCapture,
        );
        assert_eq!(
            classify_navigation(&plan, "https://accounts.identity.example/authorize"),
            NavigationDecision::AllowWithoutCapture,
        );
        for blocked in [
            "http://relay.example/login",
            "file:///tmp/payload.html",
            "tauri://localhost",
        ] {
            assert_eq!(
                classify_navigation(&plan, blocked),
                NavigationDecision::Block,
                "navigation to {blocked} was not blocked",
            );
        }
    }

    #[test]
    fn origins_use_browser_canonicalization_without_accepting_repaired_input() {
        let plan = plan("account_one", &[]).unwrap();
        assert_eq!(
            classify_navigation(&plan, "https://relay.example:443/login"),
            NavigationDecision::AllowAndCapture
        );
        assert_eq!(
            classify_navigation(&plan, "https://relay.example:444/login"),
            NavigationDecision::AllowWithoutCapture
        );
        assert_eq!(
            https_origin("https://[::1]:443/login").as_deref(),
            Some("https://[::1]")
        );
        for url in [
            "https://relay.example:65536/",
            "https://relay.example:0/",
            "https://[invalid]/",
            "https://relay.example\\evil/",
            "https://relay.example\n.evil/",
            "https://@relay.example/",
            "https:relay.example",
            "https:///relay.example",
        ] {
            assert_eq!(https_origin(url), None, "accepted malformed URL {url:?}");
        }
    }

    #[test]
    fn evidence_records_build_facts_without_claiming_real_device_success() {
        let evidence = admission_evidence();
        assert!(!evidence.platform.is_empty());
        assert!(!evidence.architecture.is_empty());
        let manifest = include_str!("../../Cargo.toml");
        assert!(manifest.contains(&format!(
            "tauri = {{ version = \"={}\"",
            evidence.tauri_version
        )));
        assert!(!evidence.app_version.is_empty());
        assert_eq!(evidence.unverified_checks, REAL_DEVICE_CHECKS);
    }

    #[test]
    fn real_device_checks_are_recorded_rather_than_asserted() {
        let evidence = admission_evidence();
        assert_eq!(evidence.unverified_checks.len(), 7);
        assert!(evidence
            .unverified_checks
            .iter()
            .all(|check| !check.is_empty()));
    }
}
