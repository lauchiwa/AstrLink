use reqwest::Method;
use serde::Deserialize;
use serde_json::{json, Value};

use crate::sidecar::{validate_etag, validate_resource_id};

#[derive(Deserialize)]
#[serde(tag = "operation", rename_all = "snake_case", deny_unknown_fields)]
pub(crate) enum IdentityOperation {
    List { cursor: Option<String> },
    Get { profile_id: String },
    Create { client: String, source: String },
    Confirm { profile_id: String, etag: String },
    Discard { profile_id: String, etag: String },
    Status {},
    Arm { client: String },
    Disarm {},
}

pub(crate) struct IdentityRequest {
    pub method: Method,
    pub path: String,
    pub body: Option<Value>,
    pub etag: Option<String>,
    pub record: bool,
    pub empty: bool,
}

impl IdentityOperation {
    /// A closed local API surface: the WebView cannot choose a URL or HTTP method.
    pub(crate) fn prepare(self, service_id: &str) -> Result<IdentityRequest, String> {
        validate_resource_id(service_id)?;
        let base = format!("/control/v1/services/{service_id}");
        let profiles = format!("{base}/identity-profiles");
        let capture = format!("{base}/identity-capture");
        let mut request = IdentityRequest {
            method: Method::GET,
            path: capture.clone(),
            body: None,
            etag: None,
            record: false,
            empty: false,
        };
        match self {
            Self::List { cursor } => {
                let mut url = reqwest::Url::parse(&format!("http://localhost{profiles}"))
                    .map_err(|_| "invalid profile list path")?;
                url.query_pairs_mut().append_pair("limit", "200");
                if let Some(cursor) = cursor {
                    if cursor.is_empty() || cursor.len() > 512 {
                        return Err("invalid profile cursor".into());
                    }
                    url.query_pairs_mut().append_pair("cursor", &cursor);
                }
                request.path = format!("{}?{}", url.path(), url.query().unwrap_or_default());
            }
            Self::Get { profile_id } => {
                validate_resource_id(&profile_id)?;
                request.path = format!("{profiles}/{profile_id}");
                request.record = true;
            }
            Self::Create { client, source } => {
                validate_client(&client)?;
                if !["builtin", "subscription_import"].contains(&source.as_str()) {
                    return Err("invalid identity source".into());
                }
                request.method = Method::POST;
                request.path = profiles;
                request.body = Some(json!({"client": client, "source": source}));
                request.record = true;
            }
            Self::Confirm { profile_id, etag } => {
                validate_resource_id(&profile_id)?;
                validate_etag(&etag)?;
                request.method = Method::POST;
                request.path = format!("{profiles}/{profile_id}/confirm");
                request.body = Some(json!({}));
                request.etag = Some(etag);
                request.record = true;
            }
            Self::Discard { profile_id, etag } => {
                validate_resource_id(&profile_id)?;
                validate_etag(&etag)?;
                request.method = Method::DELETE;
                request.path = format!("{profiles}/{profile_id}");
                request.etag = Some(etag);
                request.empty = true;
            }
            Self::Status {} => {}
            Self::Arm { client } => {
                validate_client(&client)?;
                request.method = Method::PUT;
                request.body = Some(json!({"client": client, "ttl_seconds": 600}));
            }
            Self::Disarm {} => request.method = Method::DELETE,
        }
        Ok(request)
    }
}

fn validate_client(client: &str) -> Result<(), String> {
    if ["codex_cli", "claude_code", "grok_cli"].contains(&client) {
        Ok(())
    } else {
        Err("invalid identity client".into())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn closed_operations_preserve_scope_consent_and_etags() {
        let cases = [
            (
                json!({"operation":"list"}),
                Method::GET,
                "/identity-profiles?limit=200",
            ),
            (
                json!({"operation":"get", "profile_id":"identity_one"}),
                Method::GET,
                "/identity-profiles/identity_one",
            ),
            (
                json!({"operation":"create", "client":"codex_cli", "source":"builtin"}),
                Method::POST,
                "/identity-profiles",
            ),
            (
                json!({"operation":"confirm", "profile_id":"identity_one", "etag":"\"reviewed\""}),
                Method::POST,
                "/identity-profiles/identity_one/confirm",
            ),
            (
                json!({"operation":"discard", "profile_id":"identity_one", "etag":"\"reviewed\""}),
                Method::DELETE,
                "/identity-profiles/identity_one",
            ),
            (
                json!({"operation":"status"}),
                Method::GET,
                "/identity-capture",
            ),
            (
                json!({"operation":"arm", "client":"claude_code"}),
                Method::PUT,
                "/identity-capture",
            ),
            (
                json!({"operation":"disarm"}),
                Method::DELETE,
                "/identity-capture",
            ),
        ];
        for (value, method, suffix) in cases {
            let operation: IdentityOperation = serde_json::from_value(value.clone()).unwrap();
            let request = operation.prepare("service_one").unwrap();
            assert_eq!(request.method, method);
            assert_eq!(
                request.path,
                format!("/control/v1/services/service_one{suffix}")
            );
            if value["operation"] == "confirm" {
                assert_eq!(request.body, Some(json!({})));
                assert_eq!(request.etag.as_deref(), Some("\"reviewed\""));
                assert!(request.record);
            }
        }
    }

    #[test]
    fn rejects_arbitrary_paths_fields_and_capture_sources() {
        for value in [
            json!({"operation":"unknown"}),
            json!({"operation":"status", "path":"/control/v1/access-tokens"}),
            json!({"operation":"confirm", "profile_id":"identity_one"}),
            json!({"operation":"create", "client":"codex_cli", "source":"builtin", "fingerprint":{}}),
        ] {
            assert!(serde_json::from_value::<IdentityOperation>(value).is_err());
        }
        for value in [
            json!({"operation":"get", "profile_id":"../other"}),
            json!({"operation":"confirm", "profile_id":"identity_one", "etag":"bad\r\nheader"}),
            json!({"operation":"create", "client":"codex_cli", "source":"request_capture"}),
            json!({"operation":"arm", "client":"other"}),
            json!({"operation":"list", "cursor":"a".repeat(513)}),
        ] {
            assert!(serde_json::from_value::<IdentityOperation>(value)
                .unwrap()
                .prepare("service_one")
                .is_err());
        }
        assert!(IdentityOperation::Status {}
            .prepare("service_one/other")
            .is_err());
        let request = IdentityOperation::List {
            cursor: Some("x&limit=1".into()),
        }
        .prepare("service_one")
        .unwrap();
        assert!(request.path.ends_with("cursor=x%26limit%3D1"));
    }
}
