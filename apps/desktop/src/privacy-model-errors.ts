import { i18n } from "./i18n";

export interface PrivacyModelOperationError {
  message: string;
  details: string | null;
}

const controlErrors = {
  privacy_model_metadata_unavailable: "metadata",
  invalid_privacy_model: "unsupported",
  invalid_privacy_model_probe: "source",
  invalid_privacy_model_install: "unsupported",
  privacy_model_local_probe_required: "probeAgain",
  privacy_model_local_source_unavailable: "localSource",
  privacy_model_already_installed: "installed",
  privacy_model_busy: "busy",
  privacy_model_limit: "limit",
  privacy_model_not_found: "notFound",
  privacy_model_selected: "selected",
} as const;

const dryRunErrors = {
  safety_engine_unavailable: "safety.dryRunErrors.engine",
  privacy_model_not_ready: "safety.dryRunModelNotReady",
  privacy_policy_unavailable: "safety.dryRunErrors.policy",
  invalid_policy_dry_run: "safety.dryRunErrors.sample",
} as const;

function errorDetails(error: unknown): string {
  return error instanceof Error
    ? error.message.trim()
    : typeof error === "string"
      ? error.trim()
      : "";
}

// The native bridge wraps the control API's JSON error in an HTTP summary.
function controlReason<T extends Record<string, string>>(
  details: string,
  reasons: T,
): T[keyof T] | undefined {
  const code = /"code"\s*:\s*"([a-z_]+)"/.exec(details)?.[1];
  return code && Object.hasOwn(reasons, code)
    ? reasons[code as keyof T]
    : undefined;
}

function transportReason(details: string): string | undefined {
  if (/timed? out|timeout/i.test(details)) return "timeout";
  if (
    /error sending request|connection (?:refused|reset|closed)|core is not ready/i.test(
      details,
    )
  ) {
    return "connection";
  }
  return undefined;
}

function modelErrorMessage(reason: string | undefined, fallback: string) {
  return reason
    ? i18n.t(`safety.modelErrors.${reason}`)
    : i18n.t("safety.modelErrors.fallback", { operation: fallback });
}

export function privacyModelOperationError(
  error: unknown,
  fallback: string,
): PrivacyModelOperationError {
  const details = errorDetails(error);
  let reason: string | undefined = controlReason(details, controlErrors);
  if (
    !reason &&
    (/^privacy model (?:probe|catalog|installation|adapter)\b/i.test(details) ||
      details.startsWith("Invalid privacy-policy IPC response"))
  ) {
    reason = "response";
  }
  reason ??= transportReason(details);
  return {
    message: modelErrorMessage(reason, fallback),
    details: details || null,
  };
}

/** A readable reason for a failed dry run, with the raw error kept aside. */
export function privacyDryRunError(error: unknown): PrivacyModelOperationError {
  const details = errorDetails(error);
  const key = controlReason(details, dryRunErrors);
  return {
    message: key
      ? i18n.t(key)
      : modelErrorMessage(
          transportReason(details),
          i18n.t("safety.dryRunFailed"),
        ),
    details: details || null,
  };
}
