//! Run explicitly with `cargo run --locked --example checkin_isolation_probe`.
//! Uses real Wry windows, synthetic cookies and an ephemeral loopback HTTP site.
//! No Core, plugins, real accounts, default browser store or main app is started.
//! Add `--numeric-host` to reproduce the Wry IP-host cookie filtering failure.
//! On macOS 14+, `--persistent-main` gives the synthetic main a fresh UUID store
//! and deletes only that store after all probe windows have been destroyed.
//! Add `--http-cache` to require a real main-window HTTP response-cache hit
//! before testing cleanup isolation, separately from JavaScript Cache Storage.
//! Incognito HTTP cache invalidation remains unverified if no hit is observed.
//! Add `--https` with ASTRLINK_CHECKIN_PROBE_TLS_HOST, _TLS_CERT and _TLS_KEY
//! (the last two are file paths) for a loopback TLS fixture using python3. No
//! trust store is changed and normal WebKit certificate verification remains on.
//! A successful run is evidence only for the selected synthetic store/transport,
//! not the production default store, proxy compatibility, sessions or Windows.

#[path = "checkin_probe/tls_fixture.rs"]
mod tls_fixture;

use std::{
    io::{Read, Write},
    net::{TcpListener, TcpStream},
    sync::{
        atomic::{AtomicBool, AtomicI32, AtomicUsize, Ordering},
        mpsc, Arc,
    },
    thread,
    time::{Duration, Instant},
};

use serde_json::{json, Value};
use tauri::{Manager, WebviewUrl, WebviewWindow, WebviewWindowBuilder};

const WAIT: Duration = Duration::from_secs(12);

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
struct Options {
    numeric_host: bool,
    persistent_main: bool,
    http_cache: bool,
    https: bool,
}

impl Options {
    fn parse(args: impl IntoIterator<Item = String>) -> Result<Self, String> {
        let mut options = Self::default();
        for arg in args {
            match arg.as_str() {
                "--numeric-host" if !options.numeric_host => options.numeric_host = true,
                "--persistent-main" if !options.persistent_main => options.persistent_main = true,
                "--http-cache" if !options.http_cache => options.http_cache = true,
                "--https" if !options.https => options.https = true,
                _ => return Err("unknown or duplicate probe option".into()),
            }
        }
        if options.https && options.numeric_host {
            return Err("HTTPS host is selected by TLS_HOST, not --numeric-host".into());
        }
        Ok(options)
    }
}

#[derive(Clone, Copy)]
struct ProbeStore {
    main_identifier: Option<[u8; 16]>,
}

impl ProbeStore {
    fn new(options: Options, os_version: Option<&str>) -> Result<Self, String> {
        let main_identifier = if options.persistent_main {
            // Older WebKit ignores data_store_identifier and falls back to the
            // default store. Refuse that before constructing any WebView.
            if !cfg!(target_os = "macos") || !supports_named_store(os_version) {
                return Err(
                    "--persistent-main requires macOS 14+; default store fallback refused".into(),
                );
            }
            let mut id = [0; 16];
            getrandom::getrandom(&mut id).map_err(|_| "store identifier entropy unavailable")?;
            Some(id)
        } else {
            None
        };
        Ok(Self { main_identifier })
    }
}

fn supports_named_store(version: Option<&str>) -> bool {
    version
        .and_then(|v| v.split('.').next()?.parse::<u32>().ok())
        .is_some_and(|major| major >= 14)
}

fn os_version() -> Option<String> {
    #[cfg(target_os = "macos")]
    {
        let output = std::process::Command::new("/usr/bin/sw_vers")
            .arg("-productVersion")
            .output()
            .ok()?;
        if output.status.success() {
            return Some(String::from_utf8(output.stdout).ok()?.trim().to_string());
        }
    }
    None
}

struct Profiles(std::path::PathBuf);

impl Profiles {
    fn temporary() -> std::io::Result<Self> {
        let mut random = [0u8; 16];
        getrandom::getrandom(&mut random).map_err(|e| std::io::Error::other(e.to_string()))?;
        let suffix = random
            .iter()
            .map(|b| format!("{b:02x}"))
            .collect::<String>();
        let root = std::env::temp_dir().join(format!("checkin-probe-{suffix}"));
        std::fs::create_dir(&root)?;
        Ok(Self(root))
    }
}

impl Drop for Profiles {
    fn drop(&mut self) {
        if let Err(error) = std::fs::remove_dir_all(&self.0) {
            eprintln!("temporary probe profiles could not be removed: {error}");
        }
    }
}
const COMMANDS_SOURCE: &str = include_str!("../src/lib.rs");

// Exercise every registered application command name, but never its real
// implementation. Reaching this dispatcher is already a failed ACL boundary.
fn command_names() -> Vec<&'static str> {
    let block = COMMANDS_SOURCE
        .split_once(".invoke_handler(tauri::generate_handler![")
        .expect("application command registration changed")
        .1
        .split_once("])")
        .expect("application command registration ended")
        .0;
    let mut commands: Vec<_> = block
        .split(',')
        .map(str::trim)
        .filter(|name| !name.is_empty())
        .map(|name| name.rsplit("::").next().unwrap())
        .collect();
    // Not registered in production yet: check the default-deny behavior too.
    commands.push("fork_checkin_probe_operation");
    commands
}

struct Site {
    origin: String,
    stop: Arc<AtomicBool>,
    worker: Option<thread::JoinHandle<()>>,
    cache_hits: Arc<AtomicUsize>,
    http_cache_hits: Arc<[AtomicUsize; 3]>,
    plain_requests: Arc<AtomicUsize>,
    plain_cookie_leaks: Arc<AtomicUsize>,
    tls: Option<tls_fixture::TlsFixture>,
}

impl Site {
    fn start(options: Options) -> Result<Self, String> {
        let listener = TcpListener::bind("127.0.0.1:0").map_err(|e| e.to_string())?;
        listener.set_nonblocking(true).map_err(|e| e.to_string())?;
        let address = listener.local_addr().map_err(|e| e.to_string())?;
        let origin = if options.numeric_host {
            format!("http://{address}")
        } else {
            format!("http://localhost:{}", address.port())
        };
        let tls = if options.https {
            Some(tls_fixture::TlsFixture::start(address.port())?)
        } else {
            None
        };
        let origin = tls.as_ref().map_or(origin, |tls| tls.origin.clone());
        let plain_requests = Arc::new(AtomicUsize::new(0));
        let plain_cookie_leaks = Arc::new(AtomicUsize::new(0));
        let (plain_hits, leaks) = (plain_requests.clone(), plain_cookie_leaks.clone());
        let stop = Arc::new(AtomicBool::new(false));
        let cache_hits = Arc::new(AtomicUsize::new(0));
        let http_cache_hits = Arc::new(std::array::from_fn(|_| AtomicUsize::new(0)));
        let (done, hits, http_hits) = (stop.clone(), cache_hits.clone(), http_cache_hits.clone());
        let worker = thread::spawn(move || {
            while !done.load(Ordering::SeqCst) {
                match listener.accept() {
                    Ok((stream, _)) => serve(
                        stream,
                        &hits,
                        &http_hits,
                        options.https,
                        &plain_hits,
                        &leaks,
                    ),
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                        thread::sleep(Duration::from_millis(10));
                    }
                    Err(_) => break,
                }
            }
        });
        Ok(Self {
            origin,
            stop,
            worker: Some(worker),
            cache_hits,
            http_cache_hits,
            plain_requests,
            plain_cookie_leaks,
            tls,
        })
    }
}

impl Drop for Site {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(worker) = self.worker.take() {
            let _ = worker.join();
        }
    }
}

const ACCOUNTS: [&str; 3] = ["main", "one", "two"];

fn serve(
    mut stream: TcpStream,
    hits: &AtomicUsize,
    http_hits: &[AtomicUsize; 3],
    secure: bool,
    plain_hits: &AtomicUsize,
    leaks: &AtomicUsize,
) {
    let _ = stream.set_read_timeout(Some(Duration::from_secs(2)));
    let _ = stream.set_write_timeout(Some(Duration::from_secs(2)));
    let mut request = Vec::new();
    let mut chunk = [0; 1024];
    while request.len() < 8192 {
        match stream.read(&mut chunk) {
            Ok(0) | Err(_) => return,
            Ok(count) => request.extend_from_slice(&chunk[..count]),
        }
        if request.windows(4).any(|value| value == b"\r\n\r\n") {
            break;
        }
    }
    let text = String::from_utf8_lossy(&request);
    let path = text.split_whitespace().nth(1).unwrap_or("");
    let mut headers =
        "Content-Type: text/html; charset=utf-8\r\nCache-Control: no-store\r\n".to_string();
    let body = if ["/seed/main", "/seed/one", "/seed/two"].contains(&path) {
        let account = path.rsplit('/').next().unwrap();
        let secure = if secure { "; Secure" } else { "" };
        headers.push_str(&format!(
            "Set-Cookie: probe_session={account}; Path=/; HttpOnly; SameSite=Lax{secure}\r\n"
        ));
        format!("<!doctype html><title>Probe {account}</title><p>Synthetic account {account}</p>")
    } else if let Some(index) = path
        .strip_prefix("/http-cache/")
        .and_then(|account| ACCOUNTS.iter().position(|value| *value == account))
    {
        let generation = http_hits[index].fetch_add(1, Ordering::SeqCst) + 1;
        // Different immutable paths identify the three synthetic controls. A
        // cached response must retain generation 1 without contacting this site.
        headers =
            "Content-Type: application/json\r\nCache-Control: private, max-age=3600\r\n".into();
        json!({"account": ACCOUNTS[index], "generation": generation}).to_string()
    } else if path.starts_with("/cache/") {
        hits.fetch_add(1, Ordering::SeqCst);
        headers = "Content-Type: text/plain\r\nCache-Control: public, max-age=3600\r\n".to_string();
        "synthetic cached response".into()
    } else if path == "/plain-cookie-check" {
        plain_hits.fetch_add(1, Ordering::SeqCst);
        if text.lines().any(|line| {
            line.split_once(':').is_some_and(|(name, value)| {
                name.eq_ignore_ascii_case("cookie")
                    && value
                        .split(';')
                        .any(|cookie| cookie.trim().starts_with("probe_session="))
            })
        }) {
            leaks.fetch_add(1, Ordering::SeqCst);
        }
        "<!doctype html><title>Plain HTTP negative control</title>".into()
    } else if let Some(account) = path.strip_prefix("/self/") {
        let expected = format!("probe_session={account}");
        let matches = text.lines().any(|line| {
            line.split_once(':').is_some_and(|(name, value)| {
                name.eq_ignore_ascii_case("cookie") && value.trim() == expected
            })
        });
        headers = "Content-Type: application/json\r\nCache-Control: no-store\r\n".to_string();
        json!({"matches": matches}).to_string()
    } else {
        "<!doctype html><title>Empty probe</title><p>No session seeded</p>".into()
    };
    let _ = write!(
        stream,
        "HTTP/1.1 200 OK\r\n{headers}Content-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    );
}

fn evaluate(window: &WebviewWindow, script: &str) -> Result<Value, String> {
    let (sender, receiver) = mpsc::sync_channel(1);
    window
        .eval_with_callback(script, move |result| {
            let _ = sender.send(result);
        })
        .map_err(|e| e.to_string())?;
    let result = receiver
        .recv_timeout(WAIT)
        .map_err(|_| "WebView evaluation timed out")?;
    serde_json::from_str(&result).map_err(|_| "WebView returned invalid JSON".into())
}

fn wait_for(window: &WebviewWindow, script: &str) -> Result<Value, String> {
    let deadline = Instant::now() + WAIT;
    loop {
        let result = evaluate(window, script)?;
        if !result.is_null() {
            return Ok(result);
        }
        if Instant::now() >= deadline {
            return Err("WebView fixture timed out".into());
        }
        thread::sleep(Duration::from_millis(50));
    }
}

fn remote_script() -> String {
    let commands = serde_json::to_string(&command_names()).unwrap();
    format!(
        r#"
        addEventListener('DOMContentLoaded', async () => {{
            const account = location.pathname.split('/').pop();
            const invoke = window.__TAURI_INTERNALS__?.invoke;
            if (!invoke) {{ window.probeResult = {{ error: 'IPC unavailable' }}; return; }}
            const results = await Promise.all({commands}.map(async command => {{
                try {{ await invoke(command, {{}}); return false; }}
                catch (error) {{ return /not allowed|denied/i.test(String(error)); }}
            }}));
            let matches = false;
            if (location.pathname.startsWith('/seed/')) {{
                localStorage.setItem('probe', account);
                matches = (await (await fetch('/self/' + account)).json()).matches;
                const response = await fetch('/cache/' + account);
                const cache = await caches.open('probe');
                await cache.put('/cached-entry', response);
            }}
            window.probeResult = {{
                ipcDenied: results.every(Boolean), count: results.length,
                httpOnlyHidden: !document.cookie.includes('probe_session'),
                secureContext: isSecureContext && location.protocol === 'https:',
                stored: localStorage.getItem('probe'), matches
            }};
        }}, {{ once: true }});
    "#
    )
}

fn window(
    app: &tauri::AppHandle,
    label: &str,
    url: WebviewUrl,
    script: &str,
) -> Result<WebviewWindow, String> {
    eprintln!("probe stage: create {label}");
    let (loaded, ready) = mpsc::channel();
    let directory = app.state::<std::path::PathBuf>().join(label);
    std::fs::create_dir_all(&directory).map_err(|e| e.to_string())?;
    let store = *app.state::<ProbeStore>();
    let builder = WebviewWindowBuilder::new(app, label, url)
        .data_directory(directory)
        .on_page_load(move |_, event| {
            if event.event() == tauri::webview::PageLoadEvent::Finished {
                let _ = loaded.send(());
            }
        })
        .title(format!("Isolation probe: {label}"))
        .inner_size(480.0, 200.0)
        .focused(false)
        .initialization_script(script);
    let builder = if let Some(identifier) = store.main_identifier.filter(|_| label == "main") {
        builder.incognito(false).data_store_identifier(identifier)
    } else {
        builder.incognito(true)
    };
    let view = builder.build().map_err(|e| e.to_string())?;
    ready
        .recv_timeout(WAIT)
        .map_err(|_| format!("{label} page load timed out"))?;
    Ok(view)
}

fn cookies_match(
    window: &WebviewWindow,
    origin: &str,
    account: Option<&str>,
) -> Result<bool, String> {
    // This function is called only from the probe worker, never the event loop.
    let cookies = window
        .cookies_for_url(origin.parse().map_err(|_| "invalid fixture origin")?)
        .map_err(|e| e.to_string())?;
    eprintln!(
        "probe cookie counts: label={} scoped={} all={}",
        window.label(),
        cookies.len(),
        window.cookies().map_err(|e| e.to_string())?.len()
    );
    let matching: Vec<_> = cookies
        .iter()
        .filter(|c| c.name() == "probe_session")
        .collect();
    Ok(match account {
        Some(account) => {
            matching.len() == 1
                && matching[0].value() == account
                && matching[0].http_only() == Some(true)
                && (!origin.starts_with("https://") || matching[0].secure() == Some(true))
        }
        None => matching.is_empty(),
    })
}

fn navigate_fixture(view: &WebviewWindow, url: &str) -> Result<(), String> {
    view.eval(format!(
        "window.probeResult = null; location.href = {}",
        json!(url)
    ))
    .map_err(|_| "fixture navigation failed")?;
    wait_for(
        view,
        &format!(
            "location.href === {} && document.readyState === 'complete' ? true : null",
            json!(url)
        ),
    )?;
    Ok(())
}

fn check_secure_cookie_transport(view: &WebviewWindow, site: &Site) -> Result<(), String> {
    let Some(tls) = &site.tls else { return Ok(()) };
    // The same profile navigates to cleartext HTTP: a blocked mixed-content
    // fetch would not prove that the Cookie was actually withheld on the wire.
    navigate_fixture(view, &format!("{}/plain-cookie-check", tls.plain_origin))?;
    let plain = wait_for(view, "window.probeResult ?? null")?;
    check(plain["ipcDenied"] == true, "plain HTTP remote IPC denied")?;
    check(
        site.plain_requests.load(Ordering::SeqCst) == 1,
        "plain HTTP negative control reached the fixture",
    )?;
    check(
        site.plain_cookie_leaks.load(Ordering::SeqCst) == 0,
        "Secure cookie withheld from plain HTTP",
    )?;
    navigate_fixture(view, &format!("{}/empty", site.origin))?;
    let restored = wait_for(view, "window.probeResult ?? null")?;
    check(
        restored["secureContext"] == true && restored["stored"] == "one",
        "HTTPS account storage survives cleartext navigation",
    )?;
    check(
        cookies_match(view, &site.origin, Some("one"))?,
        "Secure cookie retained after cleartext negative control",
    )
}

fn check_cached_entry(view: &WebviewWindow, result_key: &str) -> Result<(), String> {
    view.eval(format!(r#"
        caches.open('probe').then(cache => cache.match('/cached-entry')).then(async response => {{
            window.{result_key} = !!response && await response.text() === 'synthetic cached response';
        }}).catch(() => window.{result_key} = false);
    "#)).map_err(|e| e.to_string())?;
    check(
        wait_for(view, &format!("window.{result_key} ?? null"))? == true,
        "Cache Storage positive control",
    )
}

fn read_http_cache(view: &WebviewWindow, account: &str) -> Result<usize, String> {
    view.eval(format!(
        r#"
        window.httpCacheResult = null;
        fetch({}, {{ cache: 'force-cache' }})
            .then(response => response.json())
            .then(value => window.httpCacheResult = {{
                matches: value.account === {}, generation: value.generation
            }})
            .catch(() => window.httpCacheResult = {{ matches: false }});
        "#,
        json!(format!("/http-cache/{account}")),
        json!(account),
    ))
    .map_err(|e| e.to_string())?;
    let result = wait_for(view, "window.httpCacheResult ?? null")?;
    check(
        result["matches"] == true,
        "HTTP cache response belongs to the fixture account",
    )?;
    result["generation"]
        .as_u64()
        .and_then(|value| usize::try_from(value).ok())
        .filter(|value| *value > 0)
        .ok_or_else(|| "invalid HTTP cache generation".into())
}

fn fetch_http_cache(view: &WebviewWindow, account: &str, generation: usize) -> Result<(), String> {
    let observed = read_http_cache(view, account)?;
    if observed != generation {
        // Only fixed fixture labels and numeric observations reach diagnostics.
        return Err(format!(
            "HTTP response-cache mismatch: window={}, expected={}, observed={}",
            view.label(),
            generation,
            observed,
        ));
    }
    Ok(())
}

fn classify_cache_control(first: usize, second: usize) -> Result<bool, String> {
    match (first, second) {
        (1, 1) => Ok(true),
        (1, 2) => Ok(false), // No hit: cannot claim that clearing this cache worked.
        _ => Err("HTTP cache positive control had an unexpected origin count".into()),
    }
}

fn http_cache_counts(site: &Site) -> [usize; 3] {
    std::array::from_fn(|index| site.http_cache_hits[index].load(Ordering::SeqCst))
}

fn destroy_window(app: &tauri::AppHandle, view: WebviewWindow) -> Result<(), String> {
    let label = view.label().to_string();
    view.destroy().map_err(|e| e.to_string())?;
    drop(view);
    let deadline = Instant::now() + WAIT;
    while app.get_webview_window(&label).is_some() {
        if Instant::now() >= deadline {
            return Err("destroy timed out".into());
        }
        thread::sleep(Duration::from_millis(50));
    }
    Ok(())
}

fn wait_until_empty(view: &WebviewWindow, origin: &str) -> Result<(), String> {
    let deadline = Instant::now() + WAIT;
    loop {
        // Do not open a cache during this check: that would recreate what the
        // native deletion is removing. Observe the original view before close.
        view.eval("window.emptyState = null; caches.keys().then(keys => { window.emptyState = {empty: keys.length === 0 && localStorage.length === 0}; }).catch(() => window.emptyState = {empty: false});")
            .map_err(|e| e.to_string())?;
        let result = wait_for(view, "window.emptyState ?? null")?;
        if result["empty"] == true
            && cookies_match(view, origin, None)?
            && view.cookies().map_err(|e| e.to_string())?.is_empty()
        {
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err("original WebView data cleanup timed out".into());
        }
        thread::sleep(Duration::from_millis(50));
    }
}

fn cleanup(app: &tauri::AppHandle, store: ProbeStore) -> Result<(), String> {
    // This standalone app has no non-fixture windows. Run on failure as well
    // as success; no UUID other than the one generated here may be removed.
    for (_, view) in app.webview_windows() {
        destroy_window(app, view)?;
    }
    if let Some(identifier) = store.main_identifier {
        #[cfg(target_os = "macos")]
        {
            tauri::async_runtime::block_on(async {
                tokio::time::timeout(WAIT, app.remove_data_store(identifier))
                    .await
                    .map_err(|_| "named store removal timed out")?
                    .map_err(|e| format!("named store removal failed: {e}"))
            })?;
        }
        #[cfg(not(target_os = "macos"))]
        {
            let _ = identifier;
            return Err("named store removal unsupported".into());
        }
    }
    Ok(())
}

fn check(value: bool, name: &str) -> Result<(), String> {
    if value {
        Ok(())
    } else {
        Err(format!("probe failed: {name}"))
    }
}

fn exercise(
    app: &tauri::AppHandle,
    site: &Site,
    dispatched: &AtomicUsize,
    options: Options,
) -> Result<Value, String> {
    // Default is incognito; the optional persistent control uses a fresh
    // named store. Neither mode accesses the operator's default store.
    let mut main = window(
        app,
        "main",
        WebviewUrl::default(),
        r#"
        addEventListener('DOMContentLoaded', async () => {
            try { await window.__TAURI_INTERNALS__.invoke('core_status', {}); window.localControl = true; }
            catch (_) { window.localControl = false; }
        }, { once: true });
    "#,
    )?;
    eprintln!("probe stage: local IPC control");
    check(
        wait_for(&main, "window.localControl ?? null")? == true,
        "local IPC positive control",
    )?;
    check(
        dispatched.load(Ordering::SeqCst) == 1,
        "local dispatcher reached",
    )?;
    // Replace the control document with the exact same remote site used by
    // the account windows; main's capability must not authorize remote IPC.
    main.eval(format!(
        "window.location.href = {}",
        json!(format!("{}/seed/main", site.origin))
    ))
    .map_err(|e| e.to_string())?;
    // main retains only its local-control init script, so run remote checks
    // explicitly once the new remote document has loaded.
    let deadline = Instant::now() + WAIT;
    while evaluate(
        &main,
        "location.pathname === '/seed/main' && document.readyState === 'complete'",
    )? != true
    {
        if Instant::now() >= deadline {
            return Err("main navigation timed out".into());
        }
        thread::sleep(Duration::from_millis(50));
    }
    main.eval(format!(
        "{}; window.dispatchEvent(new Event('DOMContentLoaded'))",
        remote_script()
    ))
    .map_err(|e| e.to_string())?;
    let one = window(
        app,
        "fork-checkin-login-one",
        WebviewUrl::External(format!("{}/seed/one", site.origin).parse().unwrap()),
        &remote_script(),
    )?;
    let two = window(
        app,
        "fork-checkin-login-two",
        WebviewUrl::External(format!("{}/seed/two", site.origin).parse().unwrap()),
        &remote_script(),
    )?;
    for (view, account) in [(&main, "main"), (&one, "one"), (&two, "two")] {
        let result = wait_for(view, "window.probeResult ?? null")?;
        check(
            result["ipcDenied"] == true && result["count"] == command_names().len(),
            "remote IPC denied",
        )?;
        check(
            result["httpOnlyHidden"] == true && result["matches"] == true,
            "HTTP-only cookie sent only to own fixture session",
        )?;
        if options.https {
            check(result["secureContext"] == true, "HTTPS secure context")?;
        }
        check(result["stored"] == account, "local storage isolated")?;
        check(
            cookies_match(view, &site.origin, Some(account))?,
            "native HTTP-only cookie isolation",
        )?;
    }
    check(
        dispatched.load(Ordering::SeqCst) == 1,
        "no remote command reached dispatcher",
    )?;
    check_secure_cookie_transport(&one, site)?;
    let cache_hits = site.cache_hits.load(Ordering::SeqCst);
    check(cache_hits == 3, "three cache positive controls")?;
    for view in [&main, &one, &two] {
        check_cached_entry(view, "cacheBefore")?;
    }
    let mut observed_cache_hits = [false; 3];
    let mut expected_http_requests = [0; 3];
    let mut main_http_reopen_hit = None;
    if options.http_cache {
        eprintln!("probe stage: HTTP response-cache positive controls");
        for (index, (view, account)) in [(&main, "main"), (&one, "one"), (&two, "two")]
            .into_iter()
            .enumerate()
        {
            let first = read_http_cache(view, account)?;
            let second = read_http_cache(view, account)?;
            observed_cache_hits[index] = classify_cache_control(first, second)?;
            expected_http_requests[index] = second;
        }
        check(
            http_cache_counts(site) == expected_http_requests,
            "HTTP cache origin counters match the observed generations",
        )?;
        check(
            observed_cache_hits[0],
            "main HTTP response cache must have a positive hit before isolation can be verified",
        )?;
    }
    let persistent = app.state::<ProbeStore>().main_identifier.is_some();
    if persistent {
        // Positive control: reopening the same named store without reseeding
        // must retain session, localStorage and Cache Storage before cleanup.
        destroy_window(app, main)?;
        main = window(
            app,
            "main",
            WebviewUrl::External(format!("{}/empty", site.origin).parse().unwrap()),
            &remote_script(),
        )?;
        let retained = wait_for(&main, "window.probeResult ?? null")?;
        check(
            retained["ipcDenied"] == true && retained["count"] == command_names().len(),
            "persistent main remote IPC denied",
        )?;
        check(
            retained["stored"] == "main" && cookies_match(&main, &site.origin, Some("main"))?,
            "named main store survives reopen",
        )?;
        check_cached_entry(&main, "reopenedCache")?;
        if options.http_cache {
            // Recreating main itself may discard its HTTP cache. Record that
            // separately; this is not the account-cleanup operation under test.
            // Require another positive hit in the now-live main before clearing
            // account one, so a refetch cannot masquerade as preserved cache.
            let generation = read_http_cache(&main, "main")?;
            main_http_reopen_hit = Some(classify_cache_control(1, generation)?);
            fetch_http_cache(&main, "main", generation)?;
            expected_http_requests[0] = generation;
            check(
                http_cache_counts(site) == expected_http_requests,
                "live main HTTP cache has a positive control after recreation",
            )?;
        }
    }
    one.clear_all_browsing_data().map_err(|e| e.to_string())?;
    wait_until_empty(&one, &site.origin)?;
    if options.http_cache {
        eprintln!("probe stage: main HTTP cache survives clearing the account window");
        if observed_cache_hits[1] {
            fetch_http_cache(&one, "one", 2)?;
            fetch_http_cache(&one, "one", 2)?;
            expected_http_requests[1] = 2;
        }
        fetch_http_cache(&main, "main", expected_http_requests[0])?;
        check(
            http_cache_counts(site) == expected_http_requests,
            "clearing the account leaves the main HTTP cache intact",
        )?;
    }
    destroy_window(app, one)?;
    let reopened = window(
        app,
        "fork-checkin-login-one",
        WebviewUrl::External(format!("{}/empty", site.origin).parse().unwrap()),
        &remote_script(),
    )?;
    let empty = wait_for(&reopened, "window.probeResult ?? null")?;
    reopened
        .eval("caches.has('probe').then(found => window.emptyCache = !found)")
        .map_err(|e| e.to_string())?;
    check(
        wait_for(&reopened, "window.emptyCache ?? null")? == true,
        "reopened Cache Storage is empty",
    )?;
    check(
        empty["stored"].is_null() && cookies_match(&reopened, &site.origin, None)?,
        "reopened profile is empty",
    )?;
    check(
        empty["ipcDenied"] == true && empty["count"] == command_names().len(),
        "reopened remote IPC rejection completed",
    )?;
    for (view, account) in [(&main, "main"), (&two, "two")] {
        check(
            cookies_match(view, &site.origin, Some(account))?,
            "other cookies survive cleanup",
        )?;
        check(
            evaluate(view, "localStorage.getItem('probe')")? == account,
            "other storage survives cleanup",
        )?;
        check_cached_entry(view, "cacheAfter")?;
    }
    check(
        site.cache_hits.load(Ordering::SeqCst) == cache_hits,
        "other Cache Storage entries survive cleanup without network requests",
    )?;
    if options.http_cache {
        eprintln!("probe stage: HTTP cache isolation after account reopen");
        if observed_cache_hits[1] {
            fetch_http_cache(&reopened, "one", 3)?;
            expected_http_requests[1] = 3;
        }
        fetch_http_cache(&main, "main", expected_http_requests[0])?;
        if observed_cache_hits[2] {
            fetch_http_cache(&two, "two", 1)?;
        }
        check(
            http_cache_counts(site) == expected_http_requests,
            "observed HTTP cache hits survive clearing and closing another account",
        )?;
    }
    check(
        dispatched.load(Ordering::SeqCst) == 1,
        "reopened remote IPC denied",
    )?;
    for view in [reopened, two, main] {
        destroy_window(app, view)?;
    }
    Ok(json!({
        "http_only_native_read": true, "same_origin_accounts_isolated": true,
        "remote_commands_denied": command_names().len(), "local_ipc_positive_control": true,
        "original_profile_cleared_before_close": true,
        "reopened_profile_empty": true, "other_cookies_storage_cache_preserved": true,
        "main_store": if persistent { "fresh named persistent store; cookie/localStorage/Cache Storage reopen verified" } else { "incognito" },
        "http_response_cache": if options.http_cache { Some(json!({
            "observed_hits_main_one_two": observed_cache_hits,
            "main_hit_after_own_reopen": main_http_reopen_hit,
            "main_preserved_after_account_clear_and_close": true,
            "cleared_account_invalidation": observed_cache_hits[1].then_some(true),
            "reopened_account_miss": observed_cache_hits[1].then_some(true),
            "other_account_preserved": observed_cache_hits[2].then_some(true),
            "origin_requests": http_cache_counts(site),
        })) } else { None },
        "https_secure_cookie_withheld_from_http": options.https.then_some(true),
        "scope": if options.https { "synthetic main and two incognito accounts; loopback HTTPS with normal WebKit certificate validation" } else { "synthetic main and two incognito accounts; loopback HTTP only" }
    }))
}

fn main() {
    let options = Options::parse(std::env::args().skip(1)).unwrap_or_else(|error| {
        eprintln!("{error}");
        std::process::exit(2);
    });
    let os_version = os_version();
    let store = ProbeStore::new(options, os_version.as_deref()).unwrap_or_else(|error| {
        eprintln!("{error}");
        std::process::exit(2);
    });
    let mut site = match Site::start(options) {
        Ok(site) => site,
        Err(error) => {
            eprintln!("{error}");
            std::process::exit(1);
        }
    };
    let dispatched = Arc::new(AtomicUsize::new(0));
    let counter = dispatched.clone();
    let status = Arc::new(AtomicI32::new(2));
    let worker_status = status.clone();
    let profiles = Profiles::temporary().expect("create temporary probe directory");
    let app = tauri::Builder::default()
        .manage(profiles.0.clone())
        .manage(store)
        .invoke_handler(move |invoke| {
            counter.fetch_add(1, Ordering::SeqCst);
            invoke.resolver.resolve(json!({"synthetic": true}));
            true
        })
        .setup(move |app| {
            let handle = app.handle().clone();
            thread::spawn(move || {
                let result = exercise(&handle, &site, &dispatched, options);
                let cleanup_result = cleanup(&handle, store);
                let tls_result = site.tls.as_mut().map(|tls| tls.stop());
                let tls_ok = match tls_result.as_ref() {
                    None => true,
                    Some(Ok(observed)) => observed.requests > 0 && observed.handshake_failures == 0 && observed.rejected_requests == 0,
                    Some(Err(_)) => false,
                };
                let passed = result.is_ok() && cleanup_result.is_ok() && tls_ok;
                let mut unverified = vec!["Windows", "Linux", "browser proxy modes", "modern dashboard sessions", "real main default store", "production command side effects"];
                if !passed || !options.https {
                    unverified.push("HTTPS/Secure cookies");
                }
                if !passed || !options.http_cache {
                    unverified.push("HTTP response cache");
                } else if result.as_ref().is_ok_and(|value| {
                    value["http_response_cache"]["cleared_account_invalidation"].is_null()
                        || value["http_response_cache"]["other_account_preserved"].is_null()
                }) {
                    unverified.push("incognito HTTP response-cache invalidation/isolation (no hit observed)");
                }
                println!("{}", json!({
                    "platform": std::env::consts::OS, "os_version": os_version, "architecture": std::env::consts::ARCH,
                    "tauri": "2.11.5", "wry": "0.55.1", "origin": site.origin, "passed": passed,
                    "result": result.as_ref().ok(), "error": result.as_ref().err(),
                    "fixture_store_removed": cleanup_result.is_ok(), "cleanup_error": cleanup_result.err(),
                    "http_cache_requested": options.http_cache,
                    "http_cache_origin_requests": http_cache_counts(&site), "unverified": unverified,
                    "production_capture_admitted": false,
                    "https_requested": options.https,
                    "tls": tls_result.as_ref().and_then(|result| result.as_ref().ok()),
                    "tls_error": tls_result.as_ref().and_then(|result| result.as_ref().err()),
                }));
                let code = if passed { 0 } else { 1 };
                worker_status.store(code, Ordering::SeqCst);
                handle.exit(code);
            });
            Ok(())
        })
        .build(tauri::generate_context!("examples/checkin_probe/tauri.conf.json"))
        .expect("build standalone probe");
    let (finished, watchdog) = mpsc::channel::<()>();
    thread::spawn(move || {
        if watchdog.recv_timeout(Duration::from_secs(90)).is_err() {
            eprintln!("standalone WebView probe exceeded its deadline");
            // Also bounds a blocked native cookie API/event loop.
            std::process::exit(2);
        }
    });
    app.run_return(|_, event| {
        if let tauri::RunEvent::ExitRequested {
            code: None, api, ..
        } = event
        {
            api.prevent_exit(); // Closing the last fixture cannot report success.
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
    fn fixture_keeps_production_csp_and_capabilities_without_auto_windows() {
        let production: Value = serde_json::from_str(include_str!("../tauri.conf.json")).unwrap();
        let fixture: Value =
            serde_json::from_str(include_str!("checkin_probe/tauri.conf.json")).unwrap();
        let capability: Value =
            serde_json::from_str(include_str!("../capabilities/default.json")).unwrap();
        assert_eq!(
            fixture["app"]["security"]["csp"],
            production["app"]["security"]["csp"]
        );
        assert_eq!(
            fixture["app"]["security"]["capabilities"],
            json!([capability["identifier"]])
        );
        assert_eq!(fixture["app"]["windows"], json!([]));
        assert!(fixture["build"]["devUrl"].is_null());
        assert_ne!(fixture["identifier"], production["identifier"]);
        let lock = include_str!("../Cargo.lock");
        assert!(lock.contains("name = \"tauri\"\nversion = \"2.11.5\""));
        assert!(lock.contains("name = \"wry\"\nversion = \"0.55.1\""));
    }

    #[test]
    fn options_and_platform_gate_refuse_silent_store_fallback() {
        assert_eq!(Options::parse(Vec::new()).unwrap(), Options::default());
        let options = Options::parse([
            "--persistent-main".into(),
            "--numeric-host".into(),
            "--http-cache".into(),
        ])
        .unwrap();
        assert!(options.persistent_main && options.numeric_host && options.http_cache);
        for args in [
            vec!["--unexpected".into()],
            vec!["--persistent-main".into(), "--persistent-main".into()],
            vec!["--http-cache".into(), "--http-cache".into()],
            vec!["--https".into(), "--https".into()],
            vec!["--https".into(), "--numeric-host".into()],
        ] {
            assert!(Options::parse(args).is_err());
        }
        for version in [None, Some(""), Some("13.7"), Some("not-a-version")] {
            assert!(!supports_named_store(version));
            assert!(ProbeStore::new(options, version).is_err());
        }
        assert!(supports_named_store(Some("14.0")));
        assert!(supports_named_store(Some("15.7.9")));
        assert!(ProbeStore::new(Options::default(), None)
            .unwrap()
            .main_identifier
            .is_none());
    }

    #[test]
    fn http_cache_fixture_counts_origin_requests_without_changing_cache_storage_controls() {
        let site = Site::start(Options::default()).unwrap();
        let address = site.origin.strip_prefix("http://").unwrap();
        for (account, generation) in [("main", 1), ("one", 1), ("two", 1), ("one", 2)] {
            let mut stream = TcpStream::connect(address).unwrap();
            stream.set_read_timeout(Some(WAIT)).unwrap();
            write!(stream, "GET /http-cache/{account} HTTP/1.1\r\nHost: {address}\r\nConnection: close\r\n\r\n").unwrap();
            let mut response = String::new();
            stream.read_to_string(&mut response).unwrap();
            let (headers, body) = response.split_once("\r\n\r\n").unwrap();
            assert!(headers.contains("Cache-Control: private, max-age=3600"));
            assert!(!headers.contains("Set-Cookie:"));
            assert_eq!(
                serde_json::from_str::<Value>(body).unwrap(),
                json!({"account": account, "generation": generation})
            );
        }
        assert_eq!(http_cache_counts(&site), [1, 2, 1]);
        assert_eq!(site.cache_hits.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn cache_admission_requires_a_positive_hit_not_merely_successful_fetches() {
        assert_eq!(classify_cache_control(1, 1), Ok(true));
        assert_eq!(classify_cache_control(1, 2), Ok(false));
        for (first, second) in [(0, 0), (0, 1), (2, 2), (1, 3)] {
            assert!(classify_cache_control(first, second).is_err());
        }
    }

    #[test]
    fn production_main_uses_default_store_not_the_named_fixture_store() {
        let production: Value = serde_json::from_str(include_str!("../tauri.conf.json")).unwrap();
        let main = production["app"]["windows"]
            .as_array()
            .unwrap()
            .iter()
            .find(|window| window["label"] == "main")
            .unwrap();
        assert!(main["incognito"].is_null() || main["incognito"] == false);
        assert!(main["dataStoreIdentifier"].is_null());
        assert!(!COMMANDS_SOURCE.contains(".data_store_identifier("));
        assert!(!COMMANDS_SOURCE.contains(".incognito("));
        // This documents the current gap, not proof that accessing or clearing
        // a real default store would be safe. The fixture must not use it.
    }

    #[test]
    fn command_matrix_tracks_production_registration() {
        let names = command_names();
        for required in [
            "core_status",
            "update_preferences",
            "copy_access_token",
            "fork_checkin_probe_operation",
        ] {
            assert!(names.contains(&required));
        }
        assert!(names.len() > 100);
    }
}
