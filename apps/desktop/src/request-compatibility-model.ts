import { i18n } from "./i18n";
import type { ServiceAuth } from "./service-model";

export interface ModelRule {
  match: string;
  headers?: Record<string, string>;
  body?: Record<string, never>;
  identity_profile?: string;
  enabled?: boolean;
}

export interface RequestCompatibility {
  extra_headers?: Record<string, string>;
  model_rules?: ModelRule[];
  identity_profile_id?: string;
}

export interface CompatibilityDraft {
  headers: string;
  rules: string;
  identity: string;
}

export function compatibilityDraft(
  value?: RequestCompatibility,
): CompatibilityDraft {
  return {
    headers: JSON.stringify(value?.extra_headers ?? {}, null, 2),
    rules: JSON.stringify(value?.model_rules ?? [], null, 2),
    identity: value?.identity_profile_id ?? "",
  };
}

export const identityClients = [
  "codex_cli",
  "claude_code",
  "grok_cli",
] as const;
export type IdentityClient = (typeof identityClients)[number];
export const identityClientLabels: Record<IdentityClient, string> = {
  codex_cli: "Codex CLI",
  claude_code: "Claude Code",
  grok_cli: "Grok CLI",
};
export type IdentitySource =
  | "builtin"
  | "subscription_import"
  | "request_capture";
export interface IdentityProfile {
  id: string;
  service_id: string;
  client: IdentityClient;
  source: IdentitySource;
  fingerprint: {
    user_agent: string;
    version: string;
    headers?: Record<string, string>;
  };
  created_at: string;
  observed_at?: string;
  confirmed_at?: string;
}
export interface IdentityProfileRecord {
  profile: IdentityProfile;
  etag: string;
}
export interface IdentityProfilePage {
  items: IdentityProfile[];
  next_cursor: string | null;
}
export interface IdentityCaptureStatus {
  service_id: string;
  armed: boolean;
  rejected: number;
  client?: IdentityClient;
  armed_at?: string;
  expires_at?: string;
  captured_profile?: string;
}

function invalid(path: string): never {
  throw new Error(i18n.t("compatibility.invalid", { path }));
}
function object(value: unknown, path: string): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value))
    invalid(path);
  return value as Record<string, unknown>;
}
function keys(value: Record<string, unknown>, allowed: string[], path: string) {
  if (Object.keys(value).some((key) => !allowed.includes(key))) invalid(path);
}
function text(
  value: unknown,
  path: string,
  limit: number,
  empty = false,
): string {
  if (typeof value !== "string" || (!empty && !value) || value.length > limit)
    invalid(path);
  return value;
}
function resourceID(value: unknown, path: string): string {
  const id = text(value, path, 96);
  if (!/^[a-z][a-z0-9_-]{2,95}$/.test(id)) invalid(path);
  return id;
}
function printable(value: unknown, path: string, limit: number): string {
  const result = text(value, path, limit, true);
  if (!/^[\x20-\x7e]*$/.test(result) || result.trim() !== result) invalid(path);
  return result;
}
const reservedHeaders = new Set(
  `authorization proxy-authorization cookie set-cookie x-api-key x-goog-api-key api-key x-xai-token-auth chatgpt-account-id oai-product-sku www-authenticate proxy-authenticate host content-length content-type content-encoding content-range transfer-encoding connection proxy-connection keep-alive upgrade te trailer expect range accept-encoding sec-websocket-key sec-websocket-version sec-websocket-protocol sec-websocket-extensions sec-websocket-accept session-id x-claude-code-session-id x-session-id x-request-id request-id idempotency-key x-stainless-retry-count x-stainless-timeout`.split(
    " ",
  ),
);
function headersAt(
  value: unknown,
  auth: ServiceAuth,
  path: string,
): Record<string, string> {
  const headers = object(value, path);
  if (Object.keys(headers).length > 32) invalid(path);
  const seen = new Set<string>();
  return Object.fromEntries(
    Object.entries(headers).map(([name, value]) => {
      const lower = name.toLowerCase();
      if (
        !/^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,128}$/.test(name) ||
        seen.has(lower) ||
        reservedHeaders.has(lower) ||
        lower.startsWith("x-astrlink-") ||
        (auth.scheme === "custom_header" &&
          lower === auth.header_name?.toLowerCase())
      )
        invalid(`${path}.${name}`);
      seen.add(lower);
      return [name, printable(value, `${path}.${name}`, 1024)];
    }),
  );
}
export function parseRequestCompatibility(
  value: RequestCompatibility | Record<string, unknown>,
  auth: ServiceAuth,
): RequestCompatibility {
  const result: RequestCompatibility = {};
  if (value.extra_headers !== undefined)
    result.extra_headers = headersAt(
      value.extra_headers,
      auth,
      "http.extra_headers",
    );
  if (value.identity_profile_id !== undefined)
    result.identity_profile_id =
      value.identity_profile_id === ""
        ? ""
        : resourceID(value.identity_profile_id, "http.identity_profile_id");
  if (value.model_rules !== undefined) {
    if (!Array.isArray(value.model_rules) || value.model_rules.length > 200)
      invalid("http.model_rules");
    const seen = new Set<string>();
    result.model_rules = value.model_rules.map((input, index) => {
      const path = `http.model_rules[${index}]`;
      const rule = object(input, path);
      keys(
        rule,
        ["match", "headers", "body", "identity_profile", "enabled"],
        path,
      );
      const match = text(rule.match, `${path}.match`, 512);
      if (
        [...match].length > 256 ||
        match.trim() !== match ||
        (match !== "*" && /[*?[\]]/.test(match)) ||
        seen.has(match)
      )
        invalid(`${path}.match`);
      seen.add(match);
      const parsed: ModelRule = { match };
      if (rule.headers !== undefined)
        parsed.headers = headersAt(rule.headers, auth, `${path}.headers`);
      if (rule.body !== undefined) {
        if (Object.keys(object(rule.body, `${path}.body`)).length)
          invalid(`${path}.body`);
        parsed.body = {};
      }
      if (rule.enabled !== undefined) {
        if (typeof rule.enabled !== "boolean") invalid(`${path}.enabled`);
        parsed.enabled = rule.enabled;
      }
      if (rule.identity_profile !== undefined)
        parsed.identity_profile =
          rule.identity_profile === ""
            ? ""
            : resourceID(rule.identity_profile, `${path}.identity_profile`);
      return parsed;
    });
  }
  return result;
}

/** Parse both buffers on every save/probe, including the currently hidden editor. */
export function compatibilityInput(
  draft: CompatibilityDraft,
  auth: ServiceAuth,
  existing?: RequestCompatibility,
): RequestCompatibility {
  const json = (source: string, path: string, fallback: string): unknown => {
    try {
      return JSON.parse(source.trim() || fallback);
    } catch {
      return invalid(path);
    }
  };
  const parsed = parseRequestCompatibility(
    {
      extra_headers: json(draft.headers, "http.extra_headers JSON", "{}"),
      model_rules: json(draft.rules, "http.model_rules JSON", "[]"),
      identity_profile_id: draft.identity,
    },
    auth,
  );
  return {
    ...(Object.keys(parsed.extra_headers ?? {}).length ||
    existing?.extra_headers
      ? { extra_headers: parsed.extra_headers }
      : {}),
    ...(parsed.model_rules?.length || existing?.model_rules
      ? { model_rules: parsed.model_rules }
      : {}),
    ...(parsed.identity_profile_id || existing?.identity_profile_id
      ? { identity_profile_id: parsed.identity_profile_id }
      : {}),
  };
}

function timestamp(value: unknown, path: string): string {
  const result = text(value, path, 64);
  if (
    !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/.test(
      result,
    ) ||
    Number.isNaN(Date.parse(result))
  )
    invalid(path);
  return result;
}
function clientAt(value: unknown): IdentityClient {
  if (!identityClients.includes(value as IdentityClient))
    invalid("identity.client");
  return value as IdentityClient;
}
const fingerprintHeaders: Record<IdentityClient, string[]> = {
  codex_cli: ["Originator"],
  claude_code: [
    "X-App",
    "X-Stainless-Lang",
    "X-Stainless-Package-Version",
    "X-Stainless-Os",
    "X-Stainless-Arch",
    "X-Stainless-Runtime",
    "X-Stainless-Runtime-Version",
  ],
  grok_cli: [],
};
export function parseIdentityProfile(
  value: unknown,
  serviceID: string,
): IdentityProfile {
  const profile = object(value, "identity");
  keys(
    profile,
    [
      "id",
      "service_id",
      "client",
      "source",
      "fingerprint",
      "created_at",
      "observed_at",
      "confirmed_at",
    ],
    "identity",
  );
  if (profile.service_id !== serviceID) invalid("identity.service_id");
  const client = clientAt(profile.client);
  if (
    !["builtin", "subscription_import", "request_capture"].includes(
      String(profile.source),
    )
  )
    invalid("identity.source");
  const fingerprint = object(profile.fingerprint, "identity.fingerprint");
  keys(
    fingerprint,
    ["user_agent", "version", "headers"],
    "identity.fingerprint",
  );
  const headers = object(
    fingerprint.headers ?? {},
    "identity.fingerprint.headers",
  );
  keys(headers, fingerprintHeaders[client], "identity.fingerprint.headers");
  const result: IdentityProfile = {
    id: resourceID(profile.id, "identity.id"),
    service_id: resourceID(serviceID, "identity.service_id"),
    client,
    source: profile.source as IdentitySource,
    fingerprint: {
      user_agent: printable(
        fingerprint.user_agent,
        "identity.user_agent",
        1024,
      ),
      version: text(fingerprint.version, "identity.version", 64),
      headers: Object.fromEntries(
        Object.entries(headers).map(([key, value]) => [
          key,
          printable(value, `identity.headers.${key}`, 256),
        ]),
      ),
    },
    created_at: timestamp(profile.created_at, "identity.created_at"),
  };
  const version =
    /^(\d+)\.(\d+)\.(\d+)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/.exec(
      result.fingerprint.version,
    );
  if (
    !result.fingerprint.user_agent ||
    !version ||
    version.slice(1, 4).some((part) => Number(part) > 0xffffffff) ||
    result.fingerprint.user_agent.split("/")[1]?.split(" ")[0] !==
      result.fingerprint.version
  )
    invalid("identity.fingerprint");
  if (profile.observed_at !== undefined)
    result.observed_at = timestamp(profile.observed_at, "identity.observed_at");
  if (profile.confirmed_at !== undefined)
    result.confirmed_at = timestamp(
      profile.confirmed_at,
      "identity.confirmed_at",
    );
  if (
    (result.source === "request_capture" && !result.observed_at) ||
    (result.source === "builtin" && result.observed_at) ||
    (result.observed_at &&
      Date.parse(result.observed_at) > Date.parse(result.created_at)) ||
    (result.confirmed_at &&
      Date.parse(result.confirmed_at) < Date.parse(result.created_at))
  )
    invalid("identity.timestamps");
  return result;
}
export function parseIdentityProfileRecord(
  value: unknown,
  serviceID: string,
): IdentityProfileRecord {
  const record = object(value, "identity.record");
  const etag = text(record.etag, "identity.etag", 73);
  if (!/^"sha256:[0-9a-f]{64}"$/.test(etag)) invalid("identity.etag");
  return { profile: parseIdentityProfile(record.profile, serviceID), etag };
}
export function parseIdentityProfilePage(
  value: unknown,
  serviceID: string,
): IdentityProfilePage {
  const page = object(value, "identity.page");
  if (!Array.isArray(page.items) || page.items.length > 200)
    invalid("identity.items");
  const items = page.items.map((item) => parseIdentityProfile(item, serviceID));
  if (new Set(items.map(({ id }) => id)).size !== items.length)
    invalid("identity.items");
  return {
    items,
    next_cursor:
      page.next_cursor === null
        ? null
        : text(page.next_cursor, "identity.next_cursor", 512),
  };
}
export function parseIdentityCapture(
  value: unknown,
  serviceID: string,
): IdentityCaptureStatus {
  const status = object(value, "identity.capture");
  keys(
    status,
    [
      "service_id",
      "armed",
      "rejected",
      "client",
      "armed_at",
      "expires_at",
      "captured_profile",
    ],
    "identity.capture",
  );
  if (
    status.service_id !== serviceID ||
    typeof status.armed !== "boolean" ||
    !Number.isSafeInteger(status.rejected) ||
    Number(status.rejected) < 0
  )
    invalid("identity.capture");
  const result: IdentityCaptureStatus = {
    service_id: serviceID,
    armed: status.armed,
    rejected: status.rejected as number,
  };
  if (status.client !== undefined) result.client = clientAt(status.client);
  if (status.armed_at !== undefined)
    result.armed_at = timestamp(status.armed_at, "identity.armed_at");
  if (status.expires_at !== undefined)
    result.expires_at = timestamp(status.expires_at, "identity.expires_at");
  if (status.captured_profile !== undefined)
    result.captured_profile = resourceID(
      status.captured_profile,
      "identity.captured_profile",
    );
  if (
    result.armed &&
    (!result.client ||
      !result.armed_at ||
      !result.expires_at ||
      result.captured_profile)
  )
    invalid("identity.capture");
  return result;
}
