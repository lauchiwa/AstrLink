//! Aggregated check-in host support.
//!
//! The C01 admission probe, the C08 operation bridge and the P01 paste
//! importer live here. An account is connected by pasting a site session the
//! person obtained in their own browser; the in-app login window and native
//! session capture are deferred and are not wired into the app.

// The probe records admission decisions and is exercised by its own tests. It
// has no caller while the native login window stays deferred.
#[allow(dead_code)]
pub mod isolation_probe;
// The importer converts the paste the P03 dialog hands over.
pub mod credential_import;
pub mod operation;

use crate::sidecar::CoreManager;
use operation::{CheckinOperation, OperationResult};
use std::sync::Arc;
use tauri::State;

/// Runs one closed check-in operation. Only the local main window may call
/// it; other app windows are refused here and remote pages have no
/// capability at all. The operator token never leaves the host.
#[tauri::command]
pub async fn fork_checkin_operation(
    window: tauri::WebviewWindow,
    operation: CheckinOperation,
    manager: State<'_, Arc<CoreManager>>,
) -> Result<OperationResult, String> {
    require_main_window(window.label())?;
    let prepared = operation.prepare()?;
    Ok(manager.fork_checkin_send(&prepared).await)
}

fn require_main_window(label: &str) -> Result<(), String> {
    if label == isolation_probe::MAIN_WINDOW_LABEL {
        Ok(())
    } else {
        Err("check-in operations are available only to the main window".into())
    }
}

#[cfg(test)]
mod tests {
    #[test]
    fn only_the_main_window_may_operate() {
        assert!(super::require_main_window("main").is_ok());
        for label in [
            "tray-popover",
            "raw-access-approval",
            "trajectory-inspector-1",
            "fork-checkin-login-acct_one",
            "Main",
            "main ",
            "",
        ] {
            assert!(super::require_main_window(label).is_err(), "{label}");
        }
    }
}
