//! Native transport for the check-in operation bridge (task C08).
//!
//! It reuses `authenticated_control_encoded` unchanged: its single transport
//! retry resends the same encoded bytes, so a write keeps its `request_id`
//! across the retry. The operator token comes from the manager's own state,
//! never from the observer control-session file. A Core without the extension
//! answers 404, which marks only this feature unavailable; Core is not
//! restarted.

use super::{percent_encode_query, CoreManager, CorePhase};
use crate::fork_checkin::operation::{OperationResult, Outcome, PreparedRequest, API_PREFIX};

impl CoreManager {
    /// Sends one prepared check-in operation and classifies the answer. The
    /// error text of a failed exchange is not passed to the window; it gets a
    /// fixed outcome and, for a write, the `request_id` to resend with.
    pub(crate) async fn fork_checkin_send(&self, prepared: &PreparedRequest) -> OperationResult {
        let ready = self.lock_inner().phase == CorePhase::Ready;
        if !ready {
            return prepared.unanswered(Outcome::CoreNotReady);
        }
        let path = checkin_control_path(prepared);
        match self
            .authenticated_control_encoded(prepared.method().clone(), &path, prepared.body(), None)
            .await
        {
            Ok((status, _etag, body)) => prepared.answer(status.as_u16(), &body),
            Err(_) => prepared.unanswered(Outcome::TransportFailed),
        }
    }
}

fn checkin_control_path(prepared: &PreparedRequest) -> String {
    let mut path = format!("{API_PREFIX}{}", prepared.route());
    for (index, (name, value)) in prepared.query().iter().enumerate() {
        path.push(if index == 0 { '?' } else { '&' });
        path.push_str(name);
        path.push('=');
        path.push_str(&percent_encode_query(value));
    }
    path
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::fork_checkin::operation::CheckinOperation;
    use serde_json::{json, Value};
    use std::io::{BufRead, Read, Write};
    use std::time::{Duration, Instant};

    /// One request as Core saw it: request line and headers, then the body.
    struct Captured {
        head: String,
        body: Vec<u8>,
    }

    impl Captured {
        fn request_line(&self) -> &str {
            self.head.lines().next().unwrap_or_default()
        }
    }

    /// A fake Core serving one scripted reply per connection. `None` reads the
    /// whole request and drops the connection unanswered, as a Core whose
    /// response was lost.
    fn fake_core(replies: Vec<Option<String>>) -> (String, std::thread::JoinHandle<Vec<Captured>>) {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let address = format!("http://{}", listener.local_addr().unwrap());
        let server = std::thread::spawn(move || {
            let deadline = Instant::now() + Duration::from_secs(10);
            let mut captured = Vec::new();
            for reply in replies {
                let mut stream = loop {
                    match listener.accept() {
                        Ok((stream, _)) => break stream,
                        Err(_) if Instant::now() < deadline => {
                            std::thread::sleep(Duration::from_millis(5));
                        }
                        Err(error) => panic!("the fake Core saw no request: {error}"),
                    }
                };
                // BSD sockets inherit O_NONBLOCK from the listener.
                stream.set_nonblocking(false).unwrap();
                stream
                    .set_read_timeout(Some(Duration::from_secs(5)))
                    .unwrap();
                let mut reader = std::io::BufReader::new(&mut stream);
                let (mut head, mut length) = (String::new(), 0);
                loop {
                    let mut line = String::new();
                    if reader.read_line(&mut line).unwrap() == 0 || line == "\r\n" {
                        break;
                    }
                    if let Some(value) = line.to_ascii_lowercase().strip_prefix("content-length:") {
                        length = value.trim().parse::<usize>().unwrap();
                    }
                    head.push_str(&line);
                }
                let mut body = vec![0; length];
                reader.read_exact(&mut body).unwrap();
                captured.push(Captured { head, body });
                if let Some(reply) = reply {
                    stream.write_all(reply.as_bytes()).unwrap();
                }
            }
            captured
        });
        (address, server)
    }
    fn reply(status: &str, body: &str) -> Option<String> {
        Some(format!(
            "HTTP/1.1 {status}\r\nContent-Type: application/json\r\nCache-Control: no-store\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
            body.len()
        ))
    }

    fn manager_for(address: String) -> CoreManager {
        let manager = CoreManager::new();
        manager.serve_control_for_tests(std::env::temp_dir(), address);
        manager
    }

    fn prepare(operation: Value) -> PreparedRequest {
        serde_json::from_value::<CheckinOperation>(operation)
            .unwrap()
            .prepare()
            .unwrap()
    }

    fn block_on<T>(future: impl std::future::Future<Output = T>) -> T {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap()
            .block_on(future)
    }

    fn sent_request_id(captured: &Captured) -> Value {
        serde_json::from_slice::<Value>(&captured.body).unwrap()["request_id"].clone()
    }

    #[test]
    fn a_lost_answer_is_retried_with_the_same_request_id_and_bytes() {
        let receipt = r#"{"id":"job_0a1b","account_id":"acct_one","action":"check_in","status":"queued","dispatched":false,"proof_source":"none","created_at":"2026-05-01T08:00:00Z","children":[]}"#;
        let (address, server) = fake_core(vec![None, reply("202 Accepted", receipt)]);
        let manager = manager_for(address);
        let prepared =
            prepare(json!({"op": "create_job", "action": "check_in", "accounts": ["acct_one"]}));
        let result = block_on(manager.fork_checkin_send(&prepared));
        let captured = server.join().unwrap();

        assert_eq!(
            captured.len(),
            2,
            "the transport retries a lost answer once"
        );
        assert_eq!(
            captured[0].request_line(),
            "POST /control/v1/extensions/checkin/jobs HTTP/1.1"
        );
        assert_eq!(captured[0].head, captured[1].head);
        assert_eq!(
            captured[0].body, captured[1].body,
            "the retry resends the same bytes"
        );
        assert_eq!(captured[0].body, prepared.body().unwrap());
        let request_id = sent_request_id(&captured[0]);
        assert_eq!(request_id, sent_request_id(&captured[1]));
        assert!(captured[0]
            .head
            .to_ascii_lowercase()
            .contains("authorization: bearer local-test-token"));

        assert_eq!(result.outcome, Outcome::Accepted);
        assert_eq!(result.http_status, Some(202));
        assert_eq!(json!(result.request_id), request_id);
        // Accepted means queued; the job's own status carries the result.
        assert_eq!(result.body["status"], json!("queued"));
    }
    #[test]
    fn a_write_lost_twice_returns_its_request_id_and_resumes_with_the_same_bytes() {
        let receipt = r#"{"id":"job_0a1b","account_id":"acct_one","action":"check_in","status":"cancelled","dispatched":false,"proof_source":"none","created_at":"2026-05-01T08:00:00Z","children":[]}"#;
        let (address, server) = fake_core(vec![None, None, reply("200 OK", receipt)]);
        let manager = manager_for(address);
        let action = json!({"op": "cancel_job", "job_id": "job_0a1b"});
        let (lost, resumed) = block_on(async {
            let lost = manager.fork_checkin_send(&prepare(action.clone())).await;
            // The window resends the same logical action with the id it got.
            let mut again = action.clone();
            again["request_id"] = json!(lost.request_id.clone());
            let resumed = manager.fork_checkin_send(&prepare(again)).await;
            (lost, resumed)
        });
        let captured = server.join().unwrap();

        assert_eq!(lost.outcome, Outcome::TransportFailed);
        assert!(lost.retryable && lost.http_status.is_none() && lost.body.is_null());
        let request_id = lost.request_id.clone().expect("a write keeps its id");
        assert_eq!(captured.len(), 3);
        for request in &captured {
            assert_eq!(
                request.request_line(),
                "POST /control/v1/extensions/checkin/jobs/job_0a1b/cancel HTTP/1.1"
            );
            assert_eq!(
                request.body, captured[0].body,
                "every attempt is byte-identical"
            );
            assert_eq!(sent_request_id(request), json!(request_id));
        }
        assert_eq!(resumed.outcome, Outcome::Accepted);
        assert_eq!(resumed.request_id, lost.request_id);
    }

    #[test]
    fn a_retried_delete_repeats_its_query_and_sends_no_body() {
        let (address, server) = fake_core(vec![None, reply("204 No Content", "")]);
        let manager = manager_for(address);
        let prepared = prepare(json!({"op": "delete_account", "account_id": "acct_one",
                                      "request_id": "request_del_01", "expected_revision": 3}));
        let result = block_on(manager.fork_checkin_send(&prepared));
        let captured = server.join().unwrap();

        assert_eq!(captured.len(), 2);
        for request in &captured {
            assert_eq!(
                request.request_line(),
                "DELETE /control/v1/extensions/checkin/accounts/acct_one?request_id=request_del_01&expected_revision=3 HTTP/1.1"
            );
            assert!(request.body.is_empty());
        }
        assert_eq!(result.outcome, Outcome::Accepted);
        assert_eq!(result.http_status, Some(204));
        assert_eq!(result.request_id.as_deref(), Some("request_del_01"));
    }

    #[test]
    fn query_values_are_percent_encoded_into_a_fixed_path() {
        let prepared = prepare(
            json!({"op": "list_jobs", "limit": 5, "cursor": "a/b c&d=e?",
                                      "account_id": "acct_one"}),
        );
        assert_eq!(
            checkin_control_path(&prepared),
            "/control/v1/extensions/checkin/jobs?limit=5&cursor=a%2Fb%20c%26d%3De%3F&account_id=acct_one"
        );
        assert_eq!(
            checkin_control_path(&prepare(json!({"op": "status"}))),
            "/control/v1/extensions/checkin/status"
        );
    }
    #[test]
    fn an_old_core_404_marks_only_the_extension_unavailable() {
        // What a Core without the extension answers for any unknown path.
        let generic = r#"{"error":{"code":"not_found","message":"control API path not found","retryable":false,"details":[]},"request_id":"req_old"}"#;
        let (address, server) = fake_core(vec![
            reply("404 Not Found", generic),
            reply("404 Not Found", generic),
        ]);
        let manager = manager_for(address);
        let generation = manager.lock_inner().generation;
        let (status, accounts) = block_on(async {
            let status = manager
                .fork_checkin_send(&prepare(json!({"op": "status"})))
                .await;
            let accounts = manager
                .fork_checkin_send(&prepare(json!({"op": "list_accounts"})))
                .await;
            (status, accounts)
        });
        let captured = server.join().unwrap();

        assert_eq!(captured.len(), 2, "a 404 answer is not retried");
        for result in [&status, &accounts] {
            assert_eq!(result.outcome, Outcome::ExtensionUnavailable);
            assert_eq!(result.http_status, Some(404));
            assert!(result.body.is_null() && result.error_code.is_none() && !result.retryable);
        }
        let inner = manager.lock_inner();
        assert_eq!(
            inner.phase,
            CorePhase::Ready,
            "Core is not restarted or failed"
        );
        assert_eq!(inner.generation, generation);
        assert!(inner.last_error.is_none());
    }

    #[test]
    fn nothing_is_sent_while_core_is_not_ready() {
        let manager = CoreManager::new();
        let prepared = prepare(json!({"op": "update_settings", "enabled": true}));
        let result = block_on(manager.fork_checkin_send(&prepared));
        assert_eq!(result.outcome, Outcome::CoreNotReady);
        assert!(result.retryable && result.http_status.is_none());
        assert!(
            result.request_id.is_some(),
            "the action can still be resumed"
        );
    }
}
