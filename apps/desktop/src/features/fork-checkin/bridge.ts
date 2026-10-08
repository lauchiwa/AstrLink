import { invoke as invokeCommand } from "@tauri-apps/api/core";

import { checkinHasKey, checkinT } from "./i18n";
import {
  CheckinUnsupportedError,
  parseCheckinAccount,
  parseCheckinAccountPage,
  parseCheckinAuthorization,
  parseCheckinJob,
  parseCheckinJobPage,
  parseCheckinSettings,
  parseCheckinStatus,
  type CheckinAccount,
  type CheckinAccountPage,
  type CheckinAuthorization,
  type CheckinJob,
  type CheckinJobPage,
  type CheckinNetwork,
  type CheckinSettings,
  type CheckinStatus,
  type JobAction,
} from "./model";

/**
 * The extension's only native command. Every function below sends one fixed
 * variant of it; there is no way to pass a URL, method or header through.
 */
const COMMAND = "fork_checkin_operation";

const requestIDPattern = /^[A-Za-z0-9_-]{8,128}$/;

export interface PageInput {
  limit?: number;
  cursor?: string;
}

export interface AccountDraftInput {
  dashboard_base_url: string;
  time_zone: string;
  network?: CheckinNetwork;
}

export interface AccountUpdateInput {
  expected_revision: number;
  dashboard_base_url?: string;
  time_zone?: string;
  network?: CheckinNetwork;
  automatic?: boolean;
  bound_services?: string[];
}

export interface JobInput {
  action: JobAction;
  accounts: string[];
  expected_revision?: number;
}

/** What the paste dialog hands over. The text is never retained here. */
export interface CompletionInput {
  session_id: string;
  pasted_cookies: string;
  dashboard_base_url: string;
  expected_revision?: number;
  claimed_user_id?: string;
}

type ReadOperation =
  | { op: "status" }
  | { op: "get_settings" }
  | ({ op: "list_accounts" } & PageInput)
  | { op: "get_account"; account_id: string }
  | ({ op: "list_jobs"; account_id?: string } & PageInput)
  | { op: "get_job"; job_id: string };

type WriteOperation =
  | { op: "update_settings"; enabled: boolean }
  | ({ op: "create_account" } & AccountDraftInput)
  | ({ op: "update_account"; account_id: string } & AccountUpdateInput)
  | { op: "delete_account"; account_id: string; expected_revision: number }
  | ({ op: "create_job" } & JobInput)
  | { op: "cancel_job"; job_id: string }
  | {
      op: "begin_authorization";
      account_id: string;
      expected_revision: number;
    }
  | ({ op: "complete_authorization" } & CompletionInput);

export type CheckinWriteOp = WriteOperation["op"];

const writeOps: ReadonlySet<string> = new Set<CheckinWriteOp>([
  "update_settings",
  "create_account",
  "update_account",
  "delete_account",
  "create_job",
  "cancel_job",
  "begin_authorization",
  "complete_authorization",
]);

/**
 * Retrying one logical write must reuse its request_id, or Core would treat
 * the retry as a second action. The handle pins the id the native side issued
 * to the exact content it was issued for: changed content starts a new action
 * instead of provoking `request_id_reused`.
 */
export interface CheckinResume {
  readonly op: CheckinWriteOp;
  readonly fingerprint: string;
  readonly requestID: string;
}

export type CheckinFailureKind =
  /** Not running inside the desktop app. */
  | "desktop_only"
  /** The connected Core has no check-in extension. */
  | "unavailable"
  /** The extension speaks another protocol version. */
  | "unsupported"
  /** The desktop refused the request before it reached Core. */
  | "refused"
  /** Core could not be reached after the transport's own retry. */
  | "transport"
  /** Core answered with its sanitized error envelope. */
  | "rejected"
  /** A response this build does not accept. */
  | "malformed";

/**
 * A failed check-in operation. The message is a stable code, never text from
 * Core, the native side or a relay; `describeCheckinError` turns it into copy.
 */
export class CheckinBridgeError extends Error {
  readonly kind: CheckinFailureKind;
  readonly code: string | null;
  readonly httpStatus: number | null;
  readonly retryable: boolean;
  /** Present only when retrying the same logical write is safe. */
  readonly resume: CheckinResume | null;

  constructor(
    kind: CheckinFailureKind,
    init: {
      code?: string;
      httpStatus?: number;
      retryable?: boolean;
      resume?: CheckinResume | null;
    } = {},
  ) {
    super(init.code ? `${kind}:${init.code}` : kind);
    this.name = "CheckinBridgeError";
    this.kind = kind;
    this.code = init.code ?? null;
    this.httpStatus = init.httpStatus ?? null;
    this.retryable = init.retryable ?? false;
    this.resume = init.resume ?? null;
  }
}

// Every outcome the native command reports; see `operation::Outcome`.
const nativeOutcomes = [
  "accepted",
  "extension_unavailable",
  "extension_unsupported",
  "rejected",
  "forbidden",
  "not_found",
  "conflict",
  "revision_conflict",
  "temporarily_unavailable",
  "failed",
  "unexpected",
  "transport_failed",
  "core_not_ready",
] as const;

type NativeOutcome = (typeof nativeOutcomes)[number];

interface NativeResult {
  outcome: NativeOutcome;
  httpStatus: number | null;
  requestID: string | null;
  body: unknown;
  errorCode: string | null;
  retryable: boolean;
}

const nativeKeys = [
  "outcome",
  "http_status",
  "request_id",
  "body",
  "error_code",
  "retryable",
];
const errorCodePattern = /^[a-z][a-z0-9_]{0,95}$/;

function hasNativeBridge(): boolean {
  return typeof window !== "undefined" && "__TAURI_INTERNALS__" in window;
}

function nullable<T>(
  object: Record<string, unknown>,
  key: string,
  accept: (value: unknown) => value is T,
): T | null | undefined {
  const value = object[key];
  if (value === null) return null;
  return accept(value) ? value : undefined;
}

/** Strict: unknown keys, outcomes or ill-formed ids make it malformed. */
function parseNativeResult(value: unknown): NativeResult | null {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    return null;
  }
  const object = value as Record<string, unknown>;
  if (
    Object.keys(object).length !== nativeKeys.length ||
    !Object.keys(object).every((key) => nativeKeys.includes(key))
  ) {
    return null;
  }
  const outcome = object.outcome;
  if (
    typeof outcome !== "string" ||
    !(nativeOutcomes as readonly string[]).includes(outcome) ||
    typeof object.retryable !== "boolean"
  ) {
    return null;
  }
  const httpStatus = nullable(
    object,
    "http_status",
    (status): status is number =>
      typeof status === "number" &&
      Number.isInteger(status) &&
      status >= 100 &&
      status <= 599,
  );
  const requestID = nullable(
    object,
    "request_id",
    (id): id is string => typeof id === "string" && requestIDPattern.test(id),
  );
  const errorCode = nullable(
    object,
    "error_code",
    (code): code is string =>
      typeof code === "string" && errorCodePattern.test(code),
  );
  if (
    httpStatus === undefined ||
    requestID === undefined ||
    errorCode === undefined
  ) {
    return null;
  }
  if (
    outcome === "accepted" &&
    (httpStatus === null || httpStatus < 200 || httpStatus > 299)
  ) {
    return null;
  }
  return {
    outcome: outcome as NativeOutcome,
    httpStatus,
    requestID,
    body: object.body ?? null,
    errorCode,
    retryable: object.retryable,
  };
}

/** Key order must not change the fingerprint of the same content. */
function stableJSON(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(stableJSON).join(",")}]`;
  if (typeof value === "object" && value !== null) {
    return `{${Object.keys(value)
      .filter((key) => (value as Record<string, unknown>)[key] !== undefined)
      .sort()
      .map(
        (key) =>
          `${JSON.stringify(key)}:${stableJSON((value as Record<string, unknown>)[key])}`,
      )
      .join(",")}}`;
  }
  return JSON.stringify(value);
}

/**
 * Fields a write's fingerprint must ignore. `pasted_cookies` is excluded for
 * the same reason Core excludes the captured session from its own completion
 * fingerprint: a session is single-use, and no digest of it belongs outside
 * the sealed row. Keeping it out also keeps the credential from living on in
 * a resume handle after the dialog cleared its textarea.
 */
const unfingerprinted: Readonly<Record<string, readonly string[]>> = {
  complete_authorization: ["pasted_cookies"],
};

function fingerprintOf(operation: ReadOperation | WriteOperation): string {
  const omit = unfingerprinted[operation.op];
  if (!omit) return stableJSON(operation);
  const subject: Record<string, unknown> = { ...operation };
  for (const key of omit) delete subject[key];
  return stableJSON(subject);
}

interface Exchange {
  status: number;
  body: unknown;
  resume: CheckinResume | null;
}

async function exchange(
  operation: ReadOperation | WriteOperation,
  resume?: CheckinResume | null,
): Promise<Exchange> {
  if (!hasNativeBridge()) throw new CheckinBridgeError("desktop_only");
  const write = writeOps.has(operation.op);
  const fingerprint = fingerprintOf(operation);
  const payload: Record<string, unknown> = { ...operation };
  const submitted =
    write &&
    resume &&
    resume.op === operation.op &&
    resume.fingerprint === fingerprint &&
    requestIDPattern.test(resume.requestID)
      ? resume
      : null;
  if (submitted) payload.request_id = submitted.requestID;

  let raw: unknown;
  try {
    raw = await invokeCommand<unknown>(COMMAND, { operation: payload });
  } catch {
    // The desktop refused locally (wrong window, invalid field). Its reason
    // is not shown: copy comes from the failure kind alone.
    throw new CheckinBridgeError("refused", { resume: submitted });
  }

  const result = parseNativeResult(raw);
  if (
    result === null ||
    (submitted !== null && result.requestID !== submitted.requestID) ||
    (write && result.outcome === "accepted" && result.requestID === null)
  ) {
    // An unusable answer must not discard a previously issued id: Core may
    // already have committed this action. Never substitute another id from
    // a malformed or mismatched reply when the operator retries.
    throw new CheckinBridgeError("malformed", { resume: submitted });
  }
  const issued: CheckinResume | null =
    write && result.requestID
      ? {
          op: operation.op as CheckinWriteOp,
          fingerprint,
          requestID: result.requestID,
        }
      : null;
  const httpStatus = result.httpStatus ?? undefined;

  switch (result.outcome) {
    case "accepted":
      return {
        status: result.httpStatus as number,
        body: result.body,
        resume: issued,
      };
    case "extension_unavailable":
      throw new CheckinBridgeError("unavailable", { httpStatus });
    case "extension_unsupported":
      throw new CheckinBridgeError("unsupported", { httpStatus });
    case "transport_failed":
    case "core_not_ready":
      // Nothing usable came back; the same action may be resent as is.
      throw new CheckinBridgeError("transport", {
        retryable: true,
        resume: issued,
      });
    case "unexpected":
      throw new CheckinBridgeError("malformed", { httpStatus, resume: issued });
    default:
      throw new CheckinBridgeError("rejected", {
        code: result.errorCode ?? result.outcome,
        httpStatus,
        retryable: result.retryable,
        // A non-retryable refusal (validation, request_id_reused, revision
        // conflict) must not be replayed under the same id.
        resume: result.retryable ? issued : null,
      });
  }
}

function parsed<T>(
  parse: (value: unknown) => T,
  { body, resume }: Exchange,
): T {
  try {
    return parse(body);
  } catch {
    throw new CheckinBridgeError("malformed", { resume });
  }
}

export type CheckinAvailability =
  | { kind: "desktop_only" }
  | { kind: "unavailable" }
  | { kind: "unsupported" }
  | { kind: "present"; status: CheckinStatus };

/**
 * The page's first and only probe. A Core without the extension, or speaking
 * another protocol version, is a state to show, not an error to throw.
 */
export async function loadCheckinAvailability(): Promise<CheckinAvailability> {
  if (!hasNativeBridge()) return { kind: "desktop_only" };
  try {
    const answer = await exchange({ op: "status" });
    return { kind: "present", status: parseCheckinStatus(answer.body) };
  } catch (error) {
    if (error instanceof CheckinUnsupportedError) {
      return { kind: "unsupported" };
    }
    if (error instanceof CheckinBridgeError) {
      if (error.kind === "unsupported") return { kind: "unsupported" };
      if (
        error.kind === "unavailable" ||
        (error.kind === "rejected" && error.httpStatus === 404)
      ) {
        return { kind: "unavailable" };
      }
      throw error;
    }
    throw new CheckinBridgeError("malformed");
  }
}

export async function getCheckinSettings(): Promise<CheckinSettings> {
  return parsed(parseCheckinSettings, await exchange({ op: "get_settings" }));
}

export async function updateCheckinSettings(
  enabled: boolean,
  resume?: CheckinResume | null,
): Promise<CheckinSettings> {
  return parsed(
    parseCheckinSettings,
    await exchange({ op: "update_settings", enabled }, resume),
  );
}

export async function listCheckinAccounts(
  page: PageInput = {},
): Promise<CheckinAccountPage> {
  return parsed(
    parseCheckinAccountPage,
    await exchange({ op: "list_accounts", ...page }),
  );
}

export async function getCheckinAccount(
  accountID: string,
): Promise<CheckinAccount> {
  return parsed(
    parseCheckinAccount,
    await exchange({ op: "get_account", account_id: accountID }),
  );
}

export async function createCheckinAccount(
  input: AccountDraftInput,
  resume?: CheckinResume | null,
): Promise<CheckinAccount> {
  return parsed(
    parseCheckinAccount,
    await exchange({ op: "create_account", ...input }, resume),
  );
}

export async function updateCheckinAccount(
  accountID: string,
  input: AccountUpdateInput,
  resume?: CheckinResume | null,
): Promise<CheckinAccount> {
  return parsed(
    parseCheckinAccount,
    await exchange(
      { op: "update_account", account_id: accountID, ...input },
      resume,
    ),
  );
}

export async function deleteCheckinAccount(
  accountID: string,
  expectedRevision: number,
  resume?: CheckinResume | null,
): Promise<void> {
  const answer = await exchange(
    {
      op: "delete_account",
      account_id: accountID,
      expected_revision: expectedRevision,
    },
    resume,
  );
  if (
    answer.status !== 204 ||
    (answer.body !== undefined && answer.body !== null)
  ) {
    throw new CheckinBridgeError("malformed", { resume: answer.resume });
  }
}

export async function listCheckinJobs(
  query: PageInput & { account_id?: string } = {},
): Promise<CheckinJobPage> {
  return parsed(
    parseCheckinJobPage,
    await exchange({ op: "list_jobs", ...query }),
  );
}

export async function getCheckinJob(jobID: string): Promise<CheckinJob> {
  return parsed(
    parseCheckinJob,
    await exchange({ op: "get_job", job_id: jobID }),
  );
}

export async function createCheckinJob(
  input: JobInput,
  resume?: CheckinResume | null,
): Promise<CheckinJob> {
  return parsed(
    parseCheckinJob,
    await exchange({ op: "create_job", ...input }, resume),
  );
}

export async function cancelCheckinJob(
  jobID: string,
  resume?: CheckinResume | null,
): Promise<CheckinJob> {
  return parsed(
    parseCheckinJob,
    await exchange({ op: "cancel_job", job_id: jobID }, resume),
  );
}

export async function beginCheckinAuthorization(
  accountID: string,
  expectedRevision: number,
  resume?: CheckinResume | null,
): Promise<CheckinAuthorization> {
  return parsed(
    parseCheckinAuthorization,
    await exchange(
      {
        op: "begin_authorization",
        account_id: accountID,
        expected_revision: expectedRevision,
      },
      resume,
    ),
  );
}

/**
 * Hands the pasted session to the host, which converts it and sends it to
 * Core. The text is passed straight through: it is never parsed, stored or
 * logged here, and nothing derived from it is kept in the resume handle.
 */
export async function completeCheckinAuthorization(
  input: CompletionInput,
  resume?: CheckinResume | null,
): Promise<CheckinAccount> {
  return parsed(
    parseCheckinAccount,
    await exchange({ op: "complete_authorization", ...input }, resume),
  );
}

/** UI copy for a failure; never includes text from Core or a relay. */
export function describeCheckinError(error: unknown): string {
  if (!(error instanceof CheckinBridgeError)) {
    return checkinT("bridge.malformed");
  }
  switch (error.kind) {
    case "desktop_only":
      return checkinT("bridge.desktopOnly");
    case "unavailable":
      return checkinT("bridge.unavailable");
    case "unsupported":
      return checkinT("bridge.unsupported");
    case "refused":
      return checkinT("bridge.refused");
    case "transport":
      return checkinT("bridge.transport");
    case "malformed":
      return checkinT("bridge.malformed");
    case "rejected": {
      const code = error.code ?? "unknown";
      return checkinHasKey(`error.${code}`)
        ? checkinT(`error.${code}`, { code })
        : checkinT("bridge.unknownCode", { code });
    }
  }
}
