// Public check-in DTOs from contracts/extensions/checkin.openapi.yaml. Every
// value crossing the bridge is parsed strictly: an unknown field, status or
// protocol version is refused rather than guessed at. No type here carries a
// session, cookie, token or relay message; the credential never reaches JS.

import { validProxyURL } from "../../service-proxy-model";

export const CHECKIN_PROTOCOL_VERSION = 1;

export type AccountState =
  | "draft"
  | "connected"
  | "auth_required"
  | "manual_required";
export type NetworkMode = "direct" | "system" | "custom";
export type JobAction = "check_in" | "status_refresh";
export type JobStatus =
  | "queued"
  | "running"
  | "success"
  | "already_checked"
  | "not_checked"
  | "auth_required"
  | "manual_required"
  | "unsupported"
  | "rate_limited"
  | "retryable_failure"
  | "uncertain"
  | "cancelled";
export type ProofSource = "submission_response" | "status_read" | "none";

export interface CheckinStatus {
  protocol_version: number;
  present: true;
  enabled: boolean;
  storage_ready: boolean;
  scheduler_running: boolean;
  last_error_code?: string;
}

export interface CheckinSettings {
  enabled: boolean;
}

export interface CheckinNetwork {
  mode: NetworkMode;
  proxy_url?: string;
}

export interface CheckinAccount {
  id: string;
  dashboard_base_url: string;
  state: AccountState;
  revision: number;
  network: CheckinNetwork;
  time_zone: string;
  automatic: boolean;
  remote_user_id?: string;
  bound_services: string[];
  config_fingerprint: string;
}

export interface CheckinAccountPage {
  items: CheckinAccount[];
  next_cursor?: string;
}

export interface CheckinReward {
  known: boolean;
  quota?: number;
  unit?: string;
}

export interface CheckinJob {
  id: string;
  account_id?: string;
  action: JobAction;
  status: JobStatus;
  dispatched: boolean;
  proof_source: ProofSource;
  site_date?: string;
  reward?: CheckinReward;
  failure_code?: string;
  created_at: string;
  finished_at?: string;
  children: CheckinJob[];
}

export interface CheckinJobPage {
  items: CheckinJob[];
  next_cursor?: string;
}

export interface CheckinAuthorization {
  session_id: string;
  account_id: string;
  expires_at: string;
  config_fingerprint: string;
  login_url: string;
}

export interface CheckinError {
  code: string;
  message: string;
  retryable: boolean;
}

type JsonObject = Record<string, unknown>;

const accountIDPattern = /^[a-z][a-z0-9_]{2,63}$/;
const jobIDPattern = /^[a-z0-9_]{3,64}$/;
const sessionIDPattern = /^[A-Za-z0-9_-]{8,128}$/;
const serviceIDPattern = /^[a-z][a-z0-9_-]{2,95}$/;
const codePattern = /^[a-z][a-z0-9_]*$/;
const fingerprintPattern = /^[0-9a-f]{64}$/;
const rfc3339Pattern =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/;
const accountStates: readonly AccountState[] = [
  "draft",
  "connected",
  "auth_required",
  "manual_required",
];
const networkModes: readonly NetworkMode[] = ["direct", "system", "custom"];
const jobActions: readonly JobAction[] = ["check_in", "status_refresh"];
const jobStatuses: readonly JobStatus[] = [
  "queued",
  "running",
  "success",
  "already_checked",
  "not_checked",
  "auth_required",
  "manual_required",
  "unsupported",
  "rate_limited",
  "retryable_failure",
  "uncertain",
  "cancelled",
];
const proofSources: readonly ProofSource[] = [
  "submission_response",
  "status_read",
  "none",
];
const terminalStatuses = new Set<JobStatus>(
  jobStatuses.filter((status) => status !== "queued" && status !== "running"),
);

function invalid(path: string, message: string): never {
  throw new Error(`Invalid check-in response at ${path}: ${message}`);
}

function objectAt(value: unknown, path: string): JsonObject {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    return invalid(path, "expected an object");
  }
  return value as JsonObject;
}

function keysAt(
  object: JsonObject,
  required: readonly string[],
  optional: readonly string[],
  path: string,
): void {
  const allowed = new Set([...required, ...optional]);
  for (const key of Object.keys(object)) {
    if (!allowed.has(key)) invalid(`${path}.${key}`, "unexpected field");
  }
  for (const key of required) {
    if (!Object.hasOwn(object, key)) invalid(`${path}.${key}`, "missing field");
  }
}

function stringAt(value: unknown, path: string, max: number): string {
  if (typeof value !== "string" || value.length === 0) {
    return invalid(path, "expected a non-empty string");
  }
  if ([...value].length > max) invalid(path, `longer than ${max} characters`);
  return value;
}

function matchAt(
  value: unknown,
  path: string,
  pattern: RegExp,
  max = 256,
): string {
  const text = stringAt(value, path, max);
  if (!pattern.test(text)) invalid(path, "unexpected format");
  return text;
}

function oneOf<T extends string>(
  value: unknown,
  path: string,
  allowed: readonly T[],
): T {
  if (typeof value !== "string" || !allowed.includes(value as T)) {
    return invalid(path, "unknown value");
  }
  return value as T;
}

function booleanAt(value: unknown, path: string): boolean {
  if (typeof value !== "boolean") invalid(path, "expected a boolean");
  return value as boolean;
}

function integerAt(value: unknown, path: string, minimum: number): number {
  if (
    typeof value !== "number" ||
    !Number.isSafeInteger(value) ||
    value < minimum
  ) {
    return invalid(path, `expected an integer of at least ${minimum}`);
  }
  return value;
}

function timestampAt(value: unknown, path: string): string {
  const text = stringAt(value, path, 64);
  if (!rfc3339Pattern.test(text) || Number.isNaN(Date.parse(text))) {
    invalid(path, "expected an RFC 3339 timestamp");
  }
  return text;
}

function httpURLAt(value: unknown, path: string): string {
  const text = stringAt(value, path, 2048);
  let url: URL;
  try {
    url = new URL(text);
  } catch {
    return invalid(path, "expected an absolute URL");
  }
  if (
    (url.protocol !== "https:" && url.protocol !== "http:") ||
    url.username !== "" ||
    url.password !== "" ||
    url.search !== "" ||
    url.hash !== ""
  ) {
    invalid(path, "expected an http(s) address without credentials or query");
  }
  return text;
}

function arrayAt(value: unknown, path: string, max: number): unknown[] {
  if (!Array.isArray(value)) return invalid(path, "expected an array");
  if (value.length > max) invalid(path, `more than ${max} items`);
  return value;
}

function cursorAt(object: JsonObject, path: string): string | undefined {
  if (!Object.hasOwn(object, "next_cursor")) return undefined;
  return stringAt(object.next_cursor, `${path}.next_cursor`, 256);
}

export function parseCheckinStatus(value: unknown): CheckinStatus {
  const object = objectAt(value, "status");
  // Check the version first: a Core speaking another contract is reported
  // as unsupported, not as a malformed response.
  if (object.protocol_version !== CHECKIN_PROTOCOL_VERSION) {
    throw new CheckinUnsupportedError(object.protocol_version);
  }
  keysAt(
    object,
    [
      "protocol_version",
      "present",
      "enabled",
      "storage_ready",
      "scheduler_running",
    ],
    ["last_error_code"],
    "status",
  );
  if (object.present !== true) invalid("status.present", "expected true");
  const status: CheckinStatus = {
    protocol_version: CHECKIN_PROTOCOL_VERSION,
    present: true,
    enabled: booleanAt(object.enabled, "status.enabled"),
    storage_ready: booleanAt(object.storage_ready, "status.storage_ready"),
    scheduler_running: booleanAt(
      object.scheduler_running,
      "status.scheduler_running",
    ),
  };
  if (Object.hasOwn(object, "last_error_code")) {
    status.last_error_code = matchAt(
      object.last_error_code,
      "status.last_error_code",
      codePattern,
      96,
    );
  }
  return status;
}

export function parseCheckinSettings(value: unknown): CheckinSettings {
  const object = objectAt(value, "settings");
  keysAt(object, ["enabled"], [], "settings");
  return { enabled: booleanAt(object.enabled, "settings.enabled") };
}

function parseNetwork(value: unknown, path: string): CheckinNetwork {
  const object = objectAt(value, path);
  keysAt(object, ["mode"], ["proxy_url"], path);
  const mode = oneOf(object.mode, `${path}.mode`, networkModes);
  const custom = Object.hasOwn(object, "proxy_url");
  if (custom !== (mode === "custom")) {
    invalid(`${path}.proxy_url`, "present only for a custom network");
  }
  if (!custom) return { mode };
  const proxyURL = stringAt(object.proxy_url, `${path}.proxy_url`, 2048);
  if (!validProxyURL(proxyURL)) {
    invalid(
      `${path}.proxy_url`,
      "expected an http(s) or socks5 proxy without credentials",
    );
  }
  return { mode, proxy_url: proxyURL };
}

export function parseCheckinAccount(
  value: unknown,
  path = "account",
): CheckinAccount {
  const object = objectAt(value, path);
  keysAt(
    object,
    [
      "id",
      "dashboard_base_url",
      "state",
      "revision",
      "network",
      "time_zone",
      "automatic",
      "bound_services",
      "config_fingerprint",
    ],
    ["remote_user_id"],
    path,
  );
  const state = oneOf(object.state, `${path}.state`, accountStates);
  const hasRemote = Object.hasOwn(object, "remote_user_id");
  // A draft has no verified remote user; every other state has one.
  if (hasRemote === (state === "draft")) {
    invalid(`${path}.remote_user_id`, "must match the account state");
  }
  const automatic = booleanAt(object.automatic, `${path}.automatic`);
  if (automatic && state !== "connected") {
    invalid(`${path}.automatic`, "only a connected account runs on its own");
  }
  const services = arrayAt(
    object.bound_services,
    `${path}.bound_services`,
    32,
  ).map((service, index) =>
    matchAt(service, `${path}.bound_services[${index}]`, serviceIDPattern),
  );
  if (new Set(services).size !== services.length) {
    invalid(`${path}.bound_services`, "duplicate service");
  }
  const account: CheckinAccount = {
    id: matchAt(object.id, `${path}.id`, accountIDPattern),
    dashboard_base_url: httpURLAt(
      object.dashboard_base_url,
      `${path}.dashboard_base_url`,
    ),
    state,
    revision: integerAt(object.revision, `${path}.revision`, 1),
    network: parseNetwork(object.network, `${path}.network`),
    time_zone: stringAt(object.time_zone, `${path}.time_zone`, 64),
    automatic,
    bound_services: services,
    config_fingerprint: matchAt(
      object.config_fingerprint,
      `${path}.config_fingerprint`,
      fingerprintPattern,
    ),
  };
  if (hasRemote) {
    account.remote_user_id = stringAt(
      object.remote_user_id,
      `${path}.remote_user_id`,
      128,
    );
  }
  return account;
}

export function parseCheckinAccountPage(value: unknown): CheckinAccountPage {
  const object = objectAt(value, "accounts");
  keysAt(object, ["items"], ["next_cursor"], "accounts");
  const page: CheckinAccountPage = {
    items: arrayAt(object.items, "accounts.items", 100).map((item, index) =>
      parseCheckinAccount(item, `accounts.items[${index}]`),
    ),
  };
  const cursor = cursorAt(object, "accounts");
  if (cursor !== undefined) page.next_cursor = cursor;
  return page;
}

function parseReward(value: unknown, path: string): CheckinReward {
  const object = objectAt(value, path);
  keysAt(object, ["known"], ["quota", "unit"], path);
  const known = booleanAt(object.known, `${path}.known`);
  const detailed =
    Object.hasOwn(object, "quota") || Object.hasOwn(object, "unit");
  if (!known) {
    // An unknown award is shown as unknown; it is never estimated.
    if (detailed) invalid(path, "an unknown reward carries no amount");
    return { known };
  }
  keysAt(object, ["known", "quota", "unit"], [], path);
  return {
    known,
    quota: integerAt(object.quota, `${path}.quota`, 0),
    unit: matchAt(object.unit, `${path}.unit`, codePattern, 32),
  };
}

export function parseCheckinJob(
  value: unknown,
  path = "job",
  depth = 0,
): CheckinJob {
  const object = objectAt(value, path);
  keysAt(
    object,
    [
      "id",
      "action",
      "status",
      "dispatched",
      "proof_source",
      "created_at",
      "children",
    ],
    ["account_id", "site_date", "reward", "failure_code", "finished_at"],
    path,
  );
  const status = oneOf(object.status, `${path}.status`, jobStatuses);
  const proof = oneOf(
    object.proof_source,
    `${path}.proof_source`,
    proofSources,
  );
  const dispatched = booleanAt(object.dispatched, `${path}.dispatched`);
  const children = arrayAt(object.children, `${path}.children`, 25);
  // A single successful job needs proof. A batch has no proof of its own:
  // its children are validated independently below.
  if (
    children.length === 0 &&
    (status === "success" || status === "already_checked") &&
    proof === "none"
  ) {
    invalid(`${path}.proof_source`, "success without proof");
  }
  if (proof === "submission_response" && !dispatched) {
    invalid(`${path}.proof_source`, "submission proof without dispatch");
  }
  // Only a batch parent has children, and a child is never a parent.
  if (
    children.length > 0 &&
    (depth > 0 || Object.hasOwn(object, "account_id"))
  ) {
    invalid(`${path}.children`, "only a batch parent has children");
  }
  if (children.length === 0 && !Object.hasOwn(object, "account_id")) {
    invalid(`${path}.account_id`, "missing field");
  }
  const job: CheckinJob = {
    id: matchAt(object.id, `${path}.id`, jobIDPattern),
    action: oneOf(object.action, `${path}.action`, jobActions),
    status,
    dispatched,
    proof_source: proof,
    created_at: timestampAt(object.created_at, `${path}.created_at`),
    children: children.map((child, index) =>
      parseCheckinJob(child, `${path}.children[${index}]`, depth + 1),
    ),
  };
  if (job.children.length > 0) {
    if (proof !== "none" || Object.hasOwn(object, "reward")) {
      invalid(path, "a batch has no proof or reward of its own");
    }
    if (
      terminalStatuses.has(status) &&
      job.children.some((child) => !terminalStatuses.has(child.status))
    ) {
      invalid(
        `${path}.status`,
        "a batch with unfinished children is not finished",
      );
    }
    if (
      (status === "success" || status === "already_checked") &&
      job.children.some((child) => jobOutcome(child) !== "done")
    ) {
      invalid(`${path}.status`, "a successful batch needs successful children");
    }
  }
  if (Object.hasOwn(object, "account_id")) {
    job.account_id = matchAt(
      object.account_id,
      `${path}.account_id`,
      accountIDPattern,
    );
  }
  if (Object.hasOwn(object, "site_date")) {
    job.site_date = stringAt(object.site_date, `${path}.site_date`, 32);
  }
  if (Object.hasOwn(object, "reward")) {
    job.reward = parseReward(object.reward, `${path}.reward`);
  }
  if (Object.hasOwn(object, "failure_code")) {
    job.failure_code = matchAt(
      object.failure_code,
      `${path}.failure_code`,
      codePattern,
      96,
    );
  }
  if (Object.hasOwn(object, "finished_at")) {
    if (!terminalStatuses.has(status)) {
      invalid(`${path}.finished_at`, "an unfinished job has no finish time");
    }
    job.finished_at = timestampAt(object.finished_at, `${path}.finished_at`);
  }
  return job;
}

export function parseCheckinJobPage(value: unknown): CheckinJobPage {
  const object = objectAt(value, "jobs");
  keysAt(object, ["items"], ["next_cursor"], "jobs");
  const page: CheckinJobPage = {
    items: arrayAt(object.items, "jobs.items", 100).map((item, index) =>
      parseCheckinJob(item, `jobs.items[${index}]`),
    ),
  };
  const cursor = cursorAt(object, "jobs");
  if (cursor !== undefined) page.next_cursor = cursor;
  return page;
}

export function parseCheckinAuthorization(
  value: unknown,
): CheckinAuthorization {
  const object = objectAt(value, "authorization");
  keysAt(
    object,
    [
      "session_id",
      "account_id",
      "expires_at",
      "config_fingerprint",
      "login_url",
    ],
    [],
    "authorization",
  );
  return {
    session_id: matchAt(
      object.session_id,
      "authorization.session_id",
      sessionIDPattern,
    ),
    account_id: matchAt(
      object.account_id,
      "authorization.account_id",
      accountIDPattern,
    ),
    expires_at: timestampAt(object.expires_at, "authorization.expires_at"),
    config_fingerprint: matchAt(
      object.config_fingerprint,
      "authorization.config_fingerprint",
      fingerprintPattern,
    ),
    login_url: httpURLAt(object.login_url, "authorization.login_url"),
  };
}

/** Parses Core's sanitized error envelope; extra detail fields are ignored. */
export function parseCheckinError(value: unknown): CheckinError {
  const envelope = objectAt(value, "error");
  const error = objectAt(envelope.error, "error.error");
  return {
    code: matchAt(error.code, "error.error.code", codePattern, 96),
    message: stringAt(error.message, "error.error.message", 1024),
    retryable: booleanAt(error.retryable, "error.error.retryable"),
  };
}

/** A Core serving another extension protocol version. */
export class CheckinUnsupportedError extends Error {
  constructor(readonly version: unknown) {
    super("This Core serves a different check-in protocol version");
    this.name = "CheckinUnsupportedError";
  }
}

export function isTerminal(status: JobStatus): boolean {
  return terminalStatuses.has(status);
}

/**
 * How a receipt reads to the operator. Only `success` and `already_checked`
 * mean the day is done; an HTTP 200 or a balance change is never success.
 * A dispatched job without proof is uncertain until a status read resolves it.
 */
export type JobOutcome =
  | "done"
  | "pending"
  | "needs_operator"
  | "uncertain"
  | "not_done"
  | "failed";

export function jobOutcome(job: CheckinJob): JobOutcome {
  switch (job.status) {
    case "success":
    case "already_checked":
      return "done";
    case "queued":
    case "running":
      return "pending";
    case "auth_required":
    case "manual_required":
      return "needs_operator";
    case "uncertain":
      return "uncertain";
    case "cancelled":
      // A dispatched submission cannot be undone by cancelling it.
      return job.dispatched ? "uncertain" : "not_done";
    case "not_checked":
      return "not_done";
    case "unsupported":
    case "rate_limited":
    case "retryable_failure":
      return "failed";
  }
}
