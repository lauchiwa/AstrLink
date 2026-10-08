//! Test-only TLS transport. No certificate bypass, keychain or DNS changes.
//! Certificate material stays in caller-supplied files and Python's TLS server;
//! only paths cross stdin. This helper is never linked into the desktop app.

use std::{
    io::{BufRead, BufReader, Write},
    net::ToSocketAddrs,
    process::{Child, ChildStdin, Command, Stdio},
    sync::mpsc,
    thread,
    time::{Duration, Instant},
};

use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

const WAIT: Duration = Duration::from_secs(5);

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Observations {
    pub handshake_failures: usize,
    pub requests: usize,
    pub rejected_requests: usize,
}

pub struct TlsFixture {
    child: Child,
    input: Option<ChildStdin>,
    output: mpsc::Receiver<Value>,
    reader: Option<thread::JoinHandle<()>>,
    pub origin: String,
    pub plain_origin: String,
    stopped: bool,
    observations: Option<Observations>,
}

pub fn valid_host(host: &str) -> bool {
    !host.is_empty()
        && host.len() <= 253
        && host
            .bytes()
            .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || b".-".contains(&byte))
}

fn resolve_loopback(host: &str) -> Result<(), String> {
    if !valid_host(host) {
        return Err("TLS fixture requires a plain lowercase hostname".into());
    }
    let host = host.to_string();
    let (sender, receiver) = mpsc::sync_channel(1);
    thread::spawn(move || {
        let result = (host.as_str(), 1).to_socket_addrs().map(|addresses| {
            let addresses: Vec<_> = addresses.collect();
            !addresses.is_empty() && addresses.iter().all(|address| address.ip().is_loopback())
        });
        let _ = sender.send(result);
    });
    match receiver.recv_timeout(WAIT) {
        Ok(Ok(true)) => Ok(()),
        _ => Err("TLS fixture hostname must resolve only to loopback; DNS was not changed".into()),
    }
}

impl TlsFixture {
    pub fn start(backend_port: u16) -> Result<Self, String> {
        let setting = |name| {
            std::env::var(name)
                .ok()
                .filter(|value| !value.is_empty() && value.len() < 2048)
                .ok_or_else(|| format!("missing or invalid {name}"))
        };
        let host = setting("ASTRLINK_CHECKIN_PROBE_TLS_HOST")?;
        resolve_loopback(&host)?;
        let config = json!({
            "host": host, "backend_port": backend_port,
            "certificate": setting("ASTRLINK_CHECKIN_PROBE_TLS_CERT")?,
            "key": setting("ASTRLINK_CHECKIN_PROBE_TLS_KEY")?,
        });
        let mut child = Command::new("python3")
            .args(["-I", "-u", "-c", include_str!("tls_fixture.py")])
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn()
            .map_err(|_| "cannot start standalone TLS fixture; python3 required")?;
        let input = child.stdin.take();
        let stdout = child
            .stdout
            .take()
            .ok_or("TLS fixture output unavailable")?;
        let (sender, output) = mpsc::sync_channel(4);
        let reader = thread::spawn(move || {
            for line in BufReader::new(stdout).lines().take(2) {
                let Ok(line) = line else { break };
                let Ok(value) = serde_json::from_str(&line) else {
                    break;
                };
                let _ = sender.send(value);
            }
        });
        let mut fixture = Self {
            child,
            input,
            output,
            reader: Some(reader),
            origin: String::new(),
            plain_origin: format!("http://{host}:{backend_port}"),
            stopped: false,
            observations: None,
        };
        writeln!(
            fixture
                .input
                .as_mut()
                .ok_or("TLS fixture input unavailable")?,
            "{config}"
        )
        .map_err(|_| "TLS fixture configuration delivery failed")?;
        let ready = fixture
            .output
            .recv_timeout(WAIT)
            .map_err(|_| "TLS fixture startup timed out")?;
        let port = ready["port"]
            .as_u64()
            .and_then(|port| u16::try_from(port).ok())
            .filter(|port| *port > 0)
            .ok_or("TLS fixture rejected configuration or certificate files")?;
        fixture.origin = format!("https://{host}:{port}");
        Ok(fixture)
    }

    pub fn stop(&mut self) -> Result<Observations, String> {
        if self.stopped {
            return self
                .observations
                .clone()
                .ok_or("TLS fixture did not report observations".into());
        }
        drop(self.input.take());
        let deadline = Instant::now() + WAIT;
        let status = loop {
            match self.child.try_wait() {
                Ok(Some(status)) => break Some(status),
                Ok(None) if Instant::now() < deadline => thread::sleep(Duration::from_millis(20)),
                _ => {
                    let _ = self.child.kill();
                    let _ = self.child.wait();
                    break None;
                }
            }
        };
        self.stopped = true;
        if let Some(reader) = self.reader.take() {
            let _ = reader.join();
        }
        if !status.is_some_and(|status| status.success()) {
            return Err("TLS fixture failed or exceeded its shutdown deadline".into());
        }
        let observations: Observations = serde_json::from_value(
            self.output
                .try_recv()
                .map_err(|_| "TLS fixture observations unavailable")?,
        )
        .map_err(|_| "TLS fixture observations invalid")?;
        self.observations = Some(observations.clone());
        Ok(observations)
    }
}

impl Drop for TlsFixture {
    fn drop(&mut self) {
        let _ = self.stop();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tls_host_refuses_urls_credentials_ports_and_injection() {
        for host in ["localhost", "127.0.0.1", "fixture.example.test"] {
            assert!(valid_host(host));
        }
        for host in [
            "",
            "https://localhost",
            "user@localhost",
            "localhost:443",
            "localhost\n",
            "::1",
            "LOCALHOST",
        ] {
            assert!(!valid_host(host));
        }
        assert!(!valid_host(&"a".repeat(254)));
        assert!(resolve_loopback("127.0.0.1").is_ok());
        assert!(resolve_loopback("8.8.8.8").is_err());
    }
}
