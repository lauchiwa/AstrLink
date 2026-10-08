//! Bounded loopback canary and proxies for the standalone network probe.
//! Every proxy can reach exactly its own canary; it is not an open proxy.

use std::{
    io::{Read, Write},
    net::{Ipv4Addr, SocketAddr, TcpListener, TcpStream},
    sync::{
        atomic::{AtomicBool, AtomicUsize, Ordering},
        Arc,
    },
    thread,
    time::{Duration, Instant},
};

const IO_TIMEOUT: Duration = Duration::from_secs(2);
const HEADER_LIMIT: usize = 8192;
const HOP_HEADER: &str = "X-Probe-Hop: loopback-fixture";

#[derive(Default)]
pub struct Counters {
    pub direct: AtomicUsize,
    pub proxied: AtomicUsize,
    pub proxy_connections: AtomicUsize,
    /// How often a client offered proxy credentials. Only counted; no header,
    /// username or password value is ever read into a report or log.
    pub credentials_offered: AtomicUsize,
}

/// What a fixture proxy does with an accepted connection.
#[derive(Clone, Copy, PartialEq, Eq)]
pub enum Behavior {
    /// Forward exactly the one synthetic canary GET.
    Forward,
    /// Fail the connection, standing in for an unreachable proxy.
    Reject,
    /// Demand credentials the probe never supplies, so the request must fail
    /// rather than silently retry without the proxy.
    RequireAuth,
}

pub struct Server {
    pub address: SocketAddr,
    stop: Arc<AtomicBool>,
    worker: Option<thread::JoinHandle<()>>,
}

impl Server {
    fn spawn(handler: impl Fn(TcpStream) + Send + 'static) -> Result<Self, String> {
        let listener = TcpListener::bind((Ipv4Addr::LOCALHOST, 0))
            .map_err(|_| "cannot bind loopback fixture")?;
        listener
            .set_nonblocking(true)
            .map_err(|_| "cannot prepare loopback listener")?;
        let address = listener
            .local_addr()
            .map_err(|_| "cannot read fixture address")?;
        let stop = Arc::new(AtomicBool::new(false));
        let done = stop.clone();
        let worker = thread::spawn(move || {
            while !done.load(Ordering::SeqCst) {
                match listener.accept() {
                    Ok((stream, _)) => {
                        // macOS can inherit O_NONBLOCK from the listener. These
                        // bounded, multi-round handshakes require blocking I/O.
                        if stream.set_nonblocking(false).is_err() {
                            continue;
                        }
                        let _ = stream.set_read_timeout(Some(IO_TIMEOUT));
                        let _ = stream.set_write_timeout(Some(IO_TIMEOUT));
                        handler(stream);
                    }
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                        thread::sleep(Duration::from_millis(10))
                    }
                    Err(_) => break,
                }
            }
        });
        Ok(Self {
            address,
            stop,
            worker: Some(worker),
        })
    }

    pub fn canary(counters: Arc<Counters>) -> Result<Self, String> {
        Self::spawn(move |mut stream| {
            let Ok(request) = read_headers(&mut stream) else {
                return;
            };
            if request.lines().next() != Some("GET /probe HTTP/1.1") {
                let _ = respond(&mut stream, "404 Not Found", "", "");
                return;
            }
            if request.lines().any(|line| line == HOP_HEADER) {
                counters.proxied.fetch_add(1, Ordering::SeqCst);
            } else {
                counters.direct.fetch_add(1, Ordering::SeqCst);
            }
            let _ = respond(
                &mut stream,
                "200 OK",
                "Content-Type: text/html\r\n",
                "<!doctype html><title>Network probe</title><p>Synthetic response</p>",
            );
        })
    }

    pub fn proxy(
        target: SocketAddr,
        kind: ProxyKind,
        behavior: Behavior,
        counters: Arc<Counters>,
    ) -> Result<Self, String> {
        if !target.ip().is_loopback() {
            return Err("proxy target must be loopback".into());
        }
        Self::spawn(move |mut stream| {
            counters.proxy_connections.fetch_add(1, Ordering::SeqCst);
            let result = match kind {
                ProxyKind::Http => http_request(&mut stream, target, behavior, &counters),
                ProxyKind::Socks5 => socks_request(&mut stream, target, behavior, &counters),
            };
            if let Ok(request) = result {
                let _ = forward(&mut stream, target, &request);
            }
        })
    }
}

impl Drop for Server {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(worker) = self.worker.take() {
            let _ = worker.join();
        }
    }
}

#[derive(Clone, Copy)]
pub enum ProxyKind {
    Http,
    Socks5,
}

impl ProxyKind {
    pub fn scheme(self) -> &'static str {
        match self {
            Self::Http => "http",
            Self::Socks5 => "socks5",
        }
    }
}

fn respond(stream: &mut TcpStream, status: &str, headers: &str, body: &str) -> Result<(), String> {
    write!(stream, "HTTP/1.1 {status}\r\n{headers}Cache-Control: no-store\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len())
        .map_err(|_| "fixture response failed".into())
}

fn read_headers(stream: &mut TcpStream) -> Result<String, String> {
    let mut bytes = Vec::new();
    let deadline = Instant::now() + IO_TIMEOUT;
    // Bound total header time as well as size, even for a trickling client.
    // Read exactly the header, without accidentally consuming tunneled bytes.
    while bytes.len() < HEADER_LIMIT {
        let remaining = deadline
            .checked_duration_since(Instant::now())
            .filter(|time| !time.is_zero())
            .ok_or("fixture header deadline exceeded")?;
        stream
            .set_read_timeout(Some(remaining))
            .map_err(|_| "fixture timeout failed")?;
        let mut byte = [0];
        stream
            .read_exact(&mut byte)
            .map_err(|_| "fixture request unavailable")?;
        bytes.push(byte[0]);
        if bytes.ends_with(b"\r\n\r\n") {
            return String::from_utf8(bytes).map_err(|_| "fixture request is not UTF-8".into());
        }
    }
    Err("fixture request exceeds header limit".into())
}

fn allowed_authority(authority: &str, target: SocketAddr) -> bool {
    authority == target.to_string()
        || authority == format!("localhost:{}", target.port())
        || authority == format!("checkin-proxy-fixture.invalid:{}", target.port())
}

/// Counts a credential offer without retaining the value.
fn note_http_credentials(request: &str, counters: &Counters) {
    if request.lines().any(|line| {
        line.split_once(':')
            .is_some_and(|(name, _)| name.eq_ignore_ascii_case("proxy-authorization"))
    }) {
        counters.credentials_offered.fetch_add(1, Ordering::SeqCst);
    }
}

fn http_request(
    stream: &mut TcpStream,
    target: SocketAddr,
    behavior: Behavior,
    counters: &Counters,
) -> Result<String, String> {
    let request = read_headers(stream)?;
    note_http_credentials(&request, counters);
    if behavior == Behavior::RequireAuth {
        respond(
            stream,
            "407 Proxy Authentication Required",
            "Proxy-Authenticate: Basic realm=\"probe-fixture\"\r\n",
            "",
        )?;
        return Err("fixture proxy demands credentials".into());
    }
    if behavior == Behavior::Reject {
        respond(stream, "502 Bad Gateway", "", "")?;
        return Err("fixture proxy rejects connections".into());
    }
    let mut line = request
        .lines()
        .next()
        .ok_or("missing proxy request line")?
        .split_whitespace();
    let method = line.next().unwrap_or("");
    let destination = line.next().unwrap_or("");
    if method == "CONNECT" && allowed_authority(destination, target) {
        stream
            .write_all(b"HTTP/1.1 200 Connection Established\r\n\r\n")
            .map_err(|_| "fixture tunnel failed")?;
        let tunneled = read_headers(stream)?;
        note_http_credentials(&tunneled, counters);
        return Ok(tunneled);
    }
    // Linux and Windows may send absolute-form HTTP instead of CONNECT.
    let authority = destination
        .strip_prefix("http://")
        .and_then(|value| value.strip_suffix("/probe"));
    if method == "GET" && authority.is_some_and(|value| allowed_authority(value, target)) {
        let (_, headers) = request.split_once("\r\n").ok_or("missing proxy headers")?;
        return Ok(format!("GET /probe HTTP/1.1\r\n{headers}"));
    }
    respond(stream, "403 Forbidden", "", "")?;
    Err("fixture proxy refuses another destination".into())
}

/// Reads an RFC 1929 username/password exchange to count that credentials were
/// offered. Lengths only; neither value is retained, returned or logged.
fn note_socks_credentials(stream: &mut TcpStream, counters: &Counters) -> Result<(), String> {
    let mut header = [0; 2];
    stream
        .read_exact(&mut header)
        .map_err(|_| "no SOCKS credential exchange")?;
    if header[0] != 1 {
        return Err("invalid SOCKS credential version".into());
    }
    let mut discard = vec![0; header[1] as usize];
    stream
        .read_exact(&mut discard)
        .map_err(|_| "truncated SOCKS credential")?;
    let mut length = [0];
    stream
        .read_exact(&mut length)
        .map_err(|_| "truncated SOCKS credential")?;
    discard.resize(length[0] as usize, 0);
    stream
        .read_exact(&mut discard)
        .map_err(|_| "truncated SOCKS credential")?;
    counters.credentials_offered.fetch_add(1, Ordering::SeqCst);
    stream
        .write_all(&[1, 1])
        .map_err(|_| "SOCKS credential rejection failed")?;
    Err("fixture SOCKS proxy refuses supplied credentials".into())
}

fn socks_request(
    stream: &mut TcpStream,
    target: SocketAddr,
    behavior: Behavior,
    counters: &Counters,
) -> Result<String, String> {
    let mut hello = [0; 2];
    stream
        .read_exact(&mut hello)
        .map_err(|_| "missing SOCKS greeting")?;
    if hello[0] != 5 || hello[1] == 0 || hello[1] > 16 {
        return Err("invalid SOCKS greeting".into());
    }
    let mut methods = vec![0; hello[1] as usize];
    stream
        .read_exact(&mut methods)
        .map_err(|_| "missing SOCKS methods")?;
    if behavior == Behavior::RequireAuth {
        if !methods.contains(&2) {
            stream
                .write_all(&[5, 255])
                .map_err(|_| "SOCKS rejection failed")?;
            return Err("client offers no SOCKS credential method".into());
        }
        stream
            .write_all(&[5, 2])
            .map_err(|_| "SOCKS credential demand failed")?;
        return note_socks_credentials(stream, counters).map(|()| String::new());
    }
    if behavior == Behavior::Reject || !methods.contains(&0) {
        stream
            .write_all(&[5, 255])
            .map_err(|_| "SOCKS rejection failed")?;
        return Err("fixture SOCKS proxy rejects connections".into());
    }
    stream
        .write_all(&[5, 0])
        .map_err(|_| "SOCKS greeting failed")?;
    let mut header = [0; 4];
    stream
        .read_exact(&mut header)
        .map_err(|_| "missing SOCKS request")?;
    if header[..3] != [5, 1, 0] {
        return Err("invalid SOCKS request".into());
    }
    let host_matches = match header[3] {
        1 => {
            let mut address = [0; 4];
            stream
                .read_exact(&mut address)
                .map_err(|_| "missing SOCKS IPv4")?;
            address == Ipv4Addr::LOCALHOST.octets()
        }
        3 => {
            let mut length = [0];
            stream
                .read_exact(&mut length)
                .map_err(|_| "missing SOCKS host length")?;
            let mut host = vec![0; length[0] as usize];
            stream
                .read_exact(&mut host)
                .map_err(|_| "missing SOCKS host")?;
            host == b"localhost" || host == b"127.0.0.1" || host == b"checkin-proxy-fixture.invalid"
        }
        _ => return Err("unsupported fixture SOCKS address".into()),
    };
    let mut port = [0; 2];
    stream
        .read_exact(&mut port)
        .map_err(|_| "missing SOCKS port")?;
    if !host_matches || u16::from_be_bytes(port) != target.port() {
        socks_reply(stream, 2)?;
        return Err("fixture SOCKS proxy refuses another destination".into());
    }
    socks_reply(stream, 0)?;
    read_headers(stream)
}

fn socks_reply(stream: &mut TcpStream, status: u8) -> Result<(), String> {
    let mut reply = [0; 10];
    reply[0] = 5;
    reply[1] = status;
    reply[3] = 1;
    stream
        .write_all(&reply)
        .map_err(|_| "SOCKS reply failed".into())
}

fn forward(client: &mut TcpStream, target: SocketAddr, request: &str) -> Result<(), String> {
    if request.lines().next() != Some("GET /probe HTTP/1.1") {
        return Err("only the fixed canary GET can be forwarded".into());
    }
    let mut upstream =
        TcpStream::connect_timeout(&target, IO_TIMEOUT).map_err(|_| "canary connection failed")?;
    upstream
        .set_read_timeout(Some(IO_TIMEOUT))
        .map_err(|_| "canary read timeout failed")?;
    upstream
        .set_write_timeout(Some(IO_TIMEOUT))
        .map_err(|_| "canary write timeout failed")?;
    // Rebuild the sole synthetic GET instead of forwarding arbitrary headers,
    // authentication, or bodies from a browser. The canary identifies this hop.
    write!(
        upstream,
        "GET /probe HTTP/1.1\r\nHost: {target}\r\n{HOP_HEADER}\r\nConnection: close\r\n\r\n"
    )
    .map_err(|_| "canary request failed")?;
    let mut response = Vec::new();
    upstream
        .take(16384)
        .read_to_end(&mut response)
        .map_err(|_| "canary response failed")?;
    client
        .write_all(&response)
        .map_err(|_| "proxy response failed".into())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn read_response(mut stream: TcpStream) -> String {
        stream.set_read_timeout(Some(IO_TIMEOUT)).unwrap();
        let mut response = String::new();
        stream.read_to_string(&mut response).unwrap();
        response
    }

    #[test]
    fn credential_demanding_proxies_block_without_revealing_any_value() {
        for (kind, expected) in [(ProxyKind::Http, "407"), (ProxyKind::Socks5, "")] {
            let counters = Arc::new(Counters::default());
            let site = Server::canary(counters.clone()).unwrap();
            let proxy =
                Server::proxy(site.address, kind, Behavior::RequireAuth, counters.clone()).unwrap();
            let mut stream = TcpStream::connect(proxy.address).unwrap();
            stream.set_read_timeout(Some(IO_TIMEOUT)).unwrap();
            match kind {
                ProxyKind::Http => {
                    write!(stream, "CONNECT {} HTTP/1.1\r\n\r\n", site.address).unwrap();
                    let response = read_response(stream);
                    assert!(response.contains(expected));
                    assert!(response.contains("Proxy-Authenticate"));
                    assert_eq!(counters.credentials_offered.load(Ordering::SeqCst), 0);
                }
                ProxyKind::Socks5 => {
                    stream.write_all(&[5, 1, 2]).unwrap();
                    let mut greeting = [0; 2];
                    stream.read_exact(&mut greeting).unwrap();
                    assert_eq!(greeting, [5, 2]);
                    stream.write_all(&[1, 1, b'u', 1, b'p']).unwrap();
                    let mut reply = [0; 2];
                    stream.read_exact(&mut reply).unwrap();
                    assert_eq!(reply, [1, 1]);
                    assert_eq!(counters.credentials_offered.load(Ordering::SeqCst), 1);
                }
            }
            assert_eq!(counters.proxy_connections.load(Ordering::SeqCst), 1);
            assert_eq!(counters.proxied.load(Ordering::SeqCst), 0);
            assert_eq!(counters.direct.load(Ordering::SeqCst), 0);
        }
    }

    #[test]
    fn socks_proxy_without_credential_support_is_refused_before_the_canary() {
        let counters = Arc::new(Counters::default());
        let site = Server::canary(counters.clone()).unwrap();
        let proxy = Server::proxy(
            site.address,
            ProxyKind::Socks5,
            Behavior::RequireAuth,
            counters.clone(),
        )
        .unwrap();
        let mut stream = TcpStream::connect(proxy.address).unwrap();
        stream.set_read_timeout(Some(IO_TIMEOUT)).unwrap();
        stream.write_all(&[5, 1, 0]).unwrap();
        let mut greeting = [0; 2];
        stream.read_exact(&mut greeting).unwrap();
        assert_eq!(greeting, [5, 255]);
        assert_eq!(counters.credentials_offered.load(Ordering::SeqCst), 0);
        assert_eq!(counters.proxied.load(Ordering::SeqCst), 0);
        assert_eq!(counters.direct.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn http_proxy_forwards_only_its_canary_and_records_the_hop() {
        let counters = Arc::new(Counters::default());
        let site = Server::canary(counters.clone()).unwrap();
        let proxy = Server::proxy(
            site.address,
            ProxyKind::Http,
            Behavior::Forward,
            counters.clone(),
        )
        .unwrap();
        let mut stream = TcpStream::connect(proxy.address).unwrap();
        write!(
            stream,
            "GET http://{}/probe HTTP/1.1\r\nHost: {}\r\n\r\n",
            site.address, site.address
        )
        .unwrap();
        assert!(read_response(stream).starts_with("HTTP/1.1 200 OK"));
        assert_eq!(counters.proxied.load(Ordering::SeqCst), 1);
        assert_eq!(counters.direct.load(Ordering::SeqCst), 0);
        let mut refused = TcpStream::connect(proxy.address).unwrap();
        refused
            .write_all(b"CONNECT other.invalid:443 HTTP/1.1\r\n\r\n")
            .unwrap();
        assert!(read_response(refused).starts_with("HTTP/1.1 403 Forbidden"));
        assert_eq!(counters.proxied.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn socks_proxy_rejects_other_hosts_and_ports_before_connecting() {
        let counters = Arc::new(Counters::default());
        let site = Server::canary(counters.clone()).unwrap();
        let proxy = Server::proxy(
            site.address,
            ProxyKind::Socks5,
            Behavior::Forward,
            counters.clone(),
        )
        .unwrap();
        let mut stream = TcpStream::connect(proxy.address).unwrap();
        stream.set_read_timeout(Some(IO_TIMEOUT)).unwrap();
        stream.write_all(&[5, 1, 0]).unwrap();
        let mut greeting = [0; 2];
        stream.read_exact(&mut greeting).unwrap();
        assert_eq!(greeting, [5, 0]);
        stream
            .write_all(&[5, 1, 0, 1, 127, 0, 0, 2, 0, 80])
            .unwrap();
        let mut reply = [0; 10];
        stream.read_exact(&mut reply).unwrap();
        assert_eq!(reply[1], 2);
        assert_eq!(counters.proxied.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn socks_proxy_tunnels_the_fixed_canary_with_a_complete_reply() {
        let counters = Arc::new(Counters::default());
        let site = Server::canary(counters.clone()).unwrap();
        let proxy = Server::proxy(
            site.address,
            ProxyKind::Socks5,
            Behavior::Forward,
            counters.clone(),
        )
        .unwrap();
        let mut stream = TcpStream::connect(proxy.address).unwrap();
        stream.set_read_timeout(Some(IO_TIMEOUT)).unwrap();
        stream.write_all(&[5, 1, 0]).unwrap();
        let mut greeting = [0; 2];
        stream.read_exact(&mut greeting).unwrap();
        assert_eq!(greeting, [5, 0]);
        stream.write_all(&[5, 1, 0, 3, 9]).unwrap();
        stream.write_all(b"localhost").unwrap();
        stream
            .write_all(&site.address.port().to_be_bytes())
            .unwrap();
        let mut reply = [0; 10];
        stream.read_exact(&mut reply).unwrap();
        assert_eq!(reply[0], 5);
        assert_eq!(reply[1], 0);
        stream
            .write_all(b"GET /probe HTTP/1.1\r\nHost: localhost\r\n\r\n")
            .unwrap();
        assert!(read_response(stream).starts_with("HTTP/1.1 200 OK"));
        assert_eq!(counters.proxied.load(Ordering::SeqCst), 1);
        assert_eq!(counters.direct.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn http_connect_is_bounded_to_the_fixed_canary() {
        let counters = Arc::new(Counters::default());
        let site = Server::canary(counters.clone()).unwrap();
        let proxy = Server::proxy(
            site.address,
            ProxyKind::Http,
            Behavior::Forward,
            counters.clone(),
        )
        .unwrap();
        let mut stream = TcpStream::connect(proxy.address).unwrap();
        write!(
            stream,
            "CONNECT localhost:{} HTTP/1.1\r\n\r\n",
            site.address.port()
        )
        .unwrap();
        assert!(read_headers(&mut stream)
            .unwrap()
            .starts_with("HTTP/1.1 200 Connection Established"));
        stream
            .write_all(b"GET /probe HTTP/1.1\r\nHost: localhost\r\n\r\n")
            .unwrap();
        assert!(read_response(stream).starts_with("HTTP/1.1 200 OK"));
        assert_eq!(counters.proxied.load(Ordering::SeqCst), 1);
        assert_eq!(counters.direct.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn rejecting_proxy_never_reaches_the_canary() {
        let counters = Arc::new(Counters::default());
        let site = Server::canary(counters.clone()).unwrap();
        let proxy = Server::proxy(
            site.address,
            ProxyKind::Http,
            Behavior::Reject,
            counters.clone(),
        )
        .unwrap();
        let mut stream = TcpStream::connect(proxy.address).unwrap();
        write!(stream, "CONNECT {} HTTP/1.1\r\n\r\n", site.address).unwrap();
        assert!(read_response(stream).starts_with("HTTP/1.1 502 Bad Gateway"));
        assert_eq!(counters.proxied.load(Ordering::SeqCst), 0);
        assert_eq!(counters.direct.load(Ordering::SeqCst), 0);
    }
}
