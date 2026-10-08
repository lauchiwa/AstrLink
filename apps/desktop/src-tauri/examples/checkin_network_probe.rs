//! Standalone real-WebView proxy admission probe. No Core or real accounts.
//!
//! Run `cargo run --locked --example checkin_network_probe` for the production
//! dependency features, then compare with `--features tauri/macos-proxy` on
//! macOS 14+. The latter is an experiment, not a change to default features.
//! Targets are ephemeral loopback canaries. `--proxy-only-target` uses a fixed
//! .invalid name resolved only by the fixture proxy, distinguishing loopback
//! bypass from a proxy feature that is disabled. This mode cannot prove that
//! the browser never attempts direct DNS fallback. No credential is used;
//! system proxies and trust stores are neither reported nor changed.

#[path = "checkin_probe/network_fixture.rs"]
mod network_fixture;

use network_fixture::{Behavior, Counters, ProxyKind, Server};
use serde::Serialize;
use serde_json::json;
use std::{
    path::PathBuf,
    sync::{
        atomic::{AtomicI32, Ordering},
        mpsc, Arc,
    },
    thread,
    time::{Duration, Instant},
};
use tauri::{Manager, WebviewUrl, WebviewWindowBuilder};

const OBSERVATION: Duration = Duration::from_secs(3);
const CLOSE_WAIT: Duration = Duration::from_secs(5);

#[derive(Clone, Copy)]
enum Expected {
    Unconfigured,
    Proxied,
    Blocked,
}

/// Counts observed for one case. Never a URL, header or credential value.
#[derive(Clone, Copy, Serialize)]
struct Snapshot {
    direct_requests: usize,
    proxied_requests: usize,
    proxy_connections: usize,
    credentials_offered: usize,
}

impl Snapshot {
    fn read(counters: &Counters) -> Self {
        Self {
            direct_requests: counters.direct.load(Ordering::SeqCst),
            proxied_requests: counters.proxied.load(Ordering::SeqCst),
            proxy_connections: counters.proxy_connections.load(Ordering::SeqCst),
            credentials_offered: counters.credentials_offered.load(Ordering::SeqCst),
        }
    }
}

#[derive(Serialize)]
struct Observation {
    case: &'static str,
    builder_accepted: bool,
    proxy_only_target: bool,
    #[serde(flatten)]
    counts: Snapshot,
    observation_ms: u128,
    passed: bool,
}

fn accepts(expected: Expected, accepted: bool, counts: Snapshot) -> bool {
    let Snapshot {
        direct_requests: direct,
        proxied_requests: proxied,
        proxy_connections: connections,
        ..
    } = counts;
    match expected {
        Expected::Unconfigured => accepted && direct > 0 && proxied == 0 && connections == 0,
        Expected::Proxied => accepted && direct == 0 && proxied > 0 && connections > 0,
        // A blocked proxy must be contacted and must not fall back to direct.
        Expected::Blocked => accepted && direct == 0 && proxied == 0 && connections > 0,
    }
}

fn build_window(
    app: &tauri::AppHandle,
    name: &str,
    site: &Server,
    proxy: Option<&Server>,
    kind: Option<ProxyKind>,
    proxy_only_target: bool,
) -> Result<tauri::WebviewWindow, tauri::Error> {
    let host = if proxy_only_target {
        "checkin-proxy-fixture.invalid"
    } else {
        "localhost"
    };
    let url = format!("http://{host}:{}/probe", site.address.port());
    let directory = app.state::<PathBuf>().join(name);
    std::fs::create_dir(&directory).map_err(tauri::Error::Io)?;
    let mut builder =
        WebviewWindowBuilder::new(app, name, WebviewUrl::External(url.parse().unwrap()))
            .incognito(true)
            .data_directory(directory)
            .title(format!("Network probe: {name}"))
            .inner_size(440.0, 180.0)
            .focused(false);
    if let (Some(proxy), Some(kind)) = (proxy, kind) {
        builder = builder.proxy_url(
            format!("{}://{}", kind.scheme(), proxy.address)
                .parse()
                .unwrap(),
        );
    }
    builder.build()
}

fn close_window(app: &tauri::AppHandle, name: &str) -> Result<(), String> {
    if let Some(window) = app.get_webview_window(name) {
        window
            .destroy()
            .map_err(|_| "cannot destroy proxy fixture window")?;
    }
    let deadline = Instant::now() + CLOSE_WAIT;
    while app.get_webview_window(name).is_some() {
        if Instant::now() >= deadline {
            return Err("proxy fixture close timed out".into());
        }
        thread::sleep(Duration::from_millis(20));
    }
    Ok(())
}

fn run_case(
    app: &tauri::AppHandle,
    name: &'static str,
    proxy: Option<(ProxyKind, Behavior)>,
    expected: Expected,
    proxy_only_target: bool,
) -> Result<Observation, String> {
    let counters = Arc::new(Counters::default());
    let site = Server::canary(counters.clone())?;
    let proxy_server = proxy
        .map(|(kind, behavior)| Server::proxy(site.address, kind, behavior, counters.clone()))
        .transpose()?;
    let proxy_only_target = proxy_only_target && proxy.is_some();
    let window = build_window(
        app,
        name,
        &site,
        proxy_server.as_ref(),
        proxy.map(|(kind, _)| kind),
        proxy_only_target,
    );
    let builder_accepted = window.is_ok();
    if window.is_ok() {
        // Keep failed navigations alive for a bounded observation interval so
        // a silently ignored proxy or a direct retry reaches the canary.
        thread::sleep(OBSERVATION);
        close_window(app, name)?;
    }
    // Drain handlers before taking the final counters. No URL/header/body is
    // emitted; only counts distinguish proxy routing from a direct fallback.
    drop(proxy_server);
    drop(site);
    let counts = Snapshot::read(&counters);
    Ok(Observation {
        case: name,
        builder_accepted,
        proxy_only_target,
        counts,
        observation_ms: OBSERVATION.as_millis(),
        passed: accepts(expected, builder_accepted, counts),
    })
}

/// Two windows live at once, each with its own proxy and canary. Neither
/// window's traffic may appear on the other's proxy or canary.
fn run_isolation_case(
    app: &tauri::AppHandle,
    proxy_only_target: bool,
) -> Result<serde_json::Value, String> {
    let names = ["isolated-first", "isolated-second"];
    let kinds = [ProxyKind::Http, ProxyKind::Socks5];
    let counters = [Arc::new(Counters::default()), Arc::new(Counters::default())];
    let sites = [
        Server::canary(counters[0].clone())?,
        Server::canary(counters[1].clone())?,
    ];
    let proxies = [
        Server::proxy(
            sites[0].address,
            kinds[0],
            Behavior::Forward,
            counters[0].clone(),
        )?,
        Server::proxy(
            sites[1].address,
            kinds[1],
            Behavior::Forward,
            counters[1].clone(),
        )?,
    ];
    let mut accepted = [false; 2];
    for index in 0..2 {
        accepted[index] = build_window(
            app,
            names[index],
            &sites[index],
            Some(&proxies[index]),
            Some(kinds[index]),
            proxy_only_target,
        )
        .is_ok();
    }
    thread::sleep(OBSERVATION);
    for name in names {
        close_window(app, name)?;
    }
    drop(proxies);
    drop(sites);
    let counts = [Snapshot::read(&counters[0]), Snapshot::read(&counters[1])];
    let passed = accepted.iter().all(|value| *value)
        && counts
            .iter()
            .all(|value| accepts(Expected::Proxied, true, *value));
    Ok(json!({
        "case": "per-window-proxy-isolation", "builder_accepted": accepted,
        "proxy_only_target": proxy_only_target, "first_http": counts[0],
        "second_socks5": counts[1], "observation_ms": OBSERVATION.as_millis(),
        "passed": passed,
        "scope": "two concurrent windows with separate proxies; not live proxy switching on one window"
    }))
}

fn exercise(app: &tauri::AppHandle, proxy_only_target: bool) -> Result<Vec<Observation>, String> {
    let cases = [
        ("unconfigured", None, Expected::Unconfigured),
        (
            "http-proxy",
            Some((ProxyKind::Http, Behavior::Forward)),
            Expected::Proxied,
        ),
        (
            "http-rejected",
            Some((ProxyKind::Http, Behavior::Reject)),
            Expected::Blocked,
        ),
        (
            "http-auth-required",
            Some((ProxyKind::Http, Behavior::RequireAuth)),
            Expected::Blocked,
        ),
        (
            "socks5-proxy",
            Some((ProxyKind::Socks5, Behavior::Forward)),
            Expected::Proxied,
        ),
        (
            "socks5-rejected",
            Some((ProxyKind::Socks5, Behavior::Reject)),
            Expected::Blocked,
        ),
        (
            "socks5-auth-required",
            Some((ProxyKind::Socks5, Behavior::RequireAuth)),
            Expected::Blocked,
        ),
    ];
    cases
        .into_iter()
        .map(|(name, proxy, expected)| {
            eprintln!("proxy probe stage: {name}");
            let observation = run_case(app, name, proxy, expected, proxy_only_target)?;
            println!("{}", json!({"observation": observation}));
            Ok(observation)
        })
        .collect()
}

struct ProfileRoot(PathBuf);

impl ProfileRoot {
    fn temporary() -> Result<Self, String> {
        let mut bytes = [0; 16];
        getrandom::getrandom(&mut bytes).map_err(|_| "fixture entropy unavailable")?;
        let suffix = bytes
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<String>();
        let root = std::env::temp_dir().join(format!("checkin-network-probe-{suffix}"));
        std::fs::create_dir(&root).map_err(|_| "cannot create fixture root")?;
        Ok(Self(root))
    }
}

impl Drop for ProfileRoot {
    fn drop(&mut self) {
        if std::fs::remove_dir_all(&self.0).is_err() {
            eprintln!("temporary network probe profile cleanup failed");
        }
    }
}

fn proxy_only_option(args: &[String]) -> Result<bool, &'static str> {
    match args {
        [] => Ok(false),
        [flag] if flag == "--proxy-only-target" => Ok(true),
        _ => Err("only --proxy-only-target is accepted; URLs and credentials are forbidden"),
    }
}

fn main() {
    let proxy_only_target = proxy_only_option(&std::env::args().skip(1).collect::<Vec<_>>())
        .unwrap_or_else(|error| {
            eprintln!("{error}");
            std::process::exit(2);
        });
    let os_version = if cfg!(target_os = "macos") {
        std::process::Command::new("/usr/bin/sw_vers")
            .arg("-productVersion")
            .output()
            .ok()
            .filter(|output| output.status.success())
            .and_then(|output| String::from_utf8(output.stdout).ok())
            .map(|version| version.trim().to_string())
    } else {
        None
    };
    if cfg!(target_os = "macos")
        && !os_version
            .as_deref()
            .and_then(|version| version.split('.').next()?.parse::<u32>().ok())
            .is_some_and(|major| major >= 14)
    {
        eprintln!("macOS proxy experiment requires a verified macOS 14+ version");
        std::process::exit(2);
    }
    let profiles = ProfileRoot::temporary().expect("create synthetic profiles");
    let status = Arc::new(AtomicI32::new(2));
    let result_status = status.clone();
    let app = tauri::Builder::default()
        .manage(profiles.0.clone())
        .setup(move |app| {
            let handle = app.handle().clone();
            thread::spawn(move || {
                let observations = exercise(&handle, proxy_only_target);
                let isolation = observations
                    .as_ref()
                    .ok()
                    .map(|_| run_isolation_case(&handle, proxy_only_target));
                let isolation_passed = match isolation.as_ref() {
                    Some(Ok(value)) => value["passed"] == true,
                    Some(Err(_)) => false,
                    None => false,
                };
                if let Some(Ok(value)) = isolation.as_ref() {
                    println!("{}", json!({"observation": value}));
                }
                let passed = observations
                    .as_ref()
                    .is_ok_and(|rows| rows.iter().all(|row| row.passed))
                    && isolation_passed;
                println!("{}", json!({
                    "platform": std::env::consts::OS, "architecture": std::env::consts::ARCH,
                    "os_version": os_version, "tauri": "2.11.5", "wry": "0.55.1",
                    "passed": passed, "production_capture_admitted": false,
                    "proxy_only_target": proxy_only_target,
                    "observations": observations.as_ref().ok(), "error": observations.as_ref().err(),
                    "isolation": isolation.as_ref().and_then(|result| result.as_ref().ok()),
                    "isolation_error": isolation.as_ref().and_then(|result| result.as_ref().err()),
                    "scope": "ephemeral loopback HTTP canaries; explicit HTTP/SOCKS5 proxies; no credentials supplied",
                    "credential_handling": "the probe never supplies proxy credentials; auth cases verify blocking, not successful authentication",
                    "unverified": ["HTTPS dashboard and Secure cookies", "successful proxy authentication with valid credentials", "system proxy mode", "direct override of a configured system proxy", "live proxy switching on one window", "direct DNS fallback for proxy-only targets", "modern dashboard session/refresh", "other platforms"]
                }));
                let code = if passed { 0 } else { 1 };
                result_status.store(code, Ordering::SeqCst);
                handle.exit(code);
            });
            Ok(())
        })
        .build(tauri::generate_context!("examples/checkin_probe/tauri.conf.json"))
        .expect("build isolated network probe");
    let (finished, watchdog) = mpsc::channel::<()>();
    thread::spawn(move || {
        if watchdog.recv_timeout(Duration::from_secs(90)).is_err() {
            eprintln!("standalone network probe exceeded its deadline");
            std::process::exit(2);
        }
    });
    app.run_return(|_, event| {
        if let tauri::RunEvent::ExitRequested {
            code: None, api, ..
        } = event
        {
            api.prevent_exit();
        }
    });
    drop(profiles);
    let _ = finished.send(());
    std::process::exit(status.load(Ordering::SeqCst));
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn options_never_accept_an_external_target_or_credentials() {
        assert_eq!(proxy_only_option(&[]), Ok(false));
        assert_eq!(proxy_only_option(&["--proxy-only-target".into()]), Ok(true));
        for args in [
            vec!["https://other.invalid".into()],
            vec!["--proxy-only-target".into(), "--proxy-only-target".into()],
        ] {
            assert!(proxy_only_option(&args).is_err());
        }
    }

    #[test]
    fn proxy_connections_alone_cannot_prove_routing_or_no_fallback() {
        let counts = |direct, proxied, connections| Snapshot {
            direct_requests: direct,
            proxied_requests: proxied,
            proxy_connections: connections,
            credentials_offered: 0,
        };
        assert!(accepts(Expected::Proxied, true, counts(0, 1, 1)));
        assert!(!accepts(Expected::Proxied, true, counts(1, 0, 1)));
        assert!(!accepts(Expected::Proxied, true, counts(1, 1, 1)));
        assert!(!accepts(Expected::Proxied, true, counts(0, 0, 1)));
        assert!(!accepts(Expected::Proxied, false, counts(0, 1, 1)));
        // An unreachable or credential-demanding proxy must block the request
        // instead of letting the browser reach the canary directly.
        assert!(accepts(Expected::Blocked, true, counts(0, 0, 1)));
        assert!(!accepts(Expected::Blocked, true, counts(0, 0, 0)));
        assert!(!accepts(Expected::Blocked, true, counts(1, 0, 1)));
        assert!(!accepts(Expected::Blocked, true, counts(0, 1, 1)));
        assert!(accepts(Expected::Unconfigured, true, counts(1, 0, 0)));
        assert!(!accepts(Expected::Unconfigured, true, counts(1, 0, 1)));
    }
}
