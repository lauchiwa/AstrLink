import { describe, expect, it } from "vitest";
import {
  compatibilityDraft,
  compatibilityInput,
  parseRequestCompatibility,
  parseIdentityProfile,
  parseIdentityCapture,
  parseIdentityProfileRecord,
} from "./request-compatibility-model";
import { parseService } from "./service-model";

const auth = { scheme: "bearer" } as const;
const userAgent =
  "codex-tui/0.156.0 (Windows 10.0.26100; x86_64) WindowsTerminal (codex-tui; 0.156.0)";
const rules = ["gpt-5.6-luna", "gpt-6-luna", "gpt-6-astra"].map((match) => ({
  match,
  body: {},
  headers: { originator: "codex_exec", "user-agent": userAgent },
}));
const profile = {
  id: "identity_one",
  service_id: "service_one",
  client: "codex_cli",
  source: "builtin",
  fingerprint: {
    user_agent: "codex_cli_rs/0.160.0",
    version: "0.160.0",
    headers: { Originator: "codex_cli_rs" },
  },
  created_at: "2026-08-01T00:00:00Z",
};

describe("request compatibility", () => {
  it("round-trips the original three rules byte-for-byte through service IPC and drafts", () => {
    const http = {
      base_url: "https://relay.example/v1",
      auth,
      model_rules: rules,
      extra_headers: { "X-Relay-Client": "fake-value" },
      identity_profile_id: "identity_one",
    };
    const service = parseService({
      id: "service_one",
      name: "Test",
      kind: "openai_compatible",
      enabled: true,
      models: [],
      capabilities: [],
      http,
      created_at: profile.created_at,
      updated_at: profile.created_at,
    });
    expect(service.http).toEqual(http);
    expect(compatibilityInput(compatibilityDraft(service.http), auth)).toEqual({
      model_rules: rules,
      extra_headers: http.extra_headers,
      identity_profile_id: "identity_one",
    });
  });
  it("preserves omitted fields and explicitly clears previously saved values", () => {
    expect(compatibilityInput(compatibilityDraft(), auth)).toEqual({});
    expect(
      compatibilityInput(compatibilityDraft(), auth, {
        model_rules: rules,
        extra_headers: { "X-Example": "fake" },
        identity_profile_id: "identity_one",
      }),
    ).toEqual({ model_rules: [], extra_headers: {}, identity_profile_id: "" });
  });
  it.each([
    { model_rules: [{ match: "gpt-*" }] },
    { model_rules: [{ match: "*", body: { model: "other" } }] },
    { model_rules: [{ match: "*", enabled: "false" }] },
    { model_rules: [{ match: "*" }, { match: "*", enabled: false }] },
    { model_rules: [{ match: "*", identity_profile: "../other" }] },
    { extra_headers: { "User-Agent": "one", "user-agent": "two" } },
    { extra_headers: { Authorization: "fake-key" } },
    { extra_headers: { "X-AstrLink-Test": "fake-value" } },
    { extra_headers: { "Content-Type": "text/plain" } },
    { extra_headers: { "Session-Id": "fixed" } },
    { extra_headers: { "X-Test": "fake\r\ninjected" } },
  ])("rejects unsafe configuration %#", (value) => {
    expect(() => parseRequestCompatibility(value, auth)).toThrow();
  });
  it("validates custom credentials and both hidden draft buffers without including their values in errors", () => {
    const draft = compatibilityDraft({
      extra_headers: { "X-Relay-Token": "fake-private-value" },
    });
    expect(() =>
      compatibilityInput(draft, {
        scheme: "custom_header",
        header_name: "x-relay-token",
      }),
    ).toThrow("http.extra_headers.X-Relay-Token");
    expect(() => compatibilityInput({ ...draft, rules: "[" }, auth)).toThrow(
      "http.model_rules JSON",
    );
    expect(() =>
      compatibilityInput({ ...draft, headers: '{"fake-private-value"' }, auth),
    ).not.toThrow("fake-private-value");
    expect(() =>
      compatibilityInput({ ...draft, headers: "null" }, auth),
    ).toThrow("http.extra_headers");
  });
  it("accepts frozen pre-release/build versions without applying subscription version floors", () => {
    for (const version of ["0.100.0", "0.160.0-alpha.1+build.2"]) {
      expect(
        parseIdentityProfile(
          {
            ...profile,
            fingerprint: {
              ...profile.fingerprint,
              version,
              user_agent: `codex_cli_rs/${version}`,
            },
          },
          "service_one",
        ).fingerprint.version,
      ).toBe(version);
    }
    expect(() =>
      parseIdentityProfile(
        {
          ...profile,
          fingerprint: {
            ...profile.fingerprint,
            version: "4294967296.0.0",
            user_agent: "codex_cli_rs/4294967296.0.0",
          },
        },
        "service_one",
      ),
    ).toThrow();
  });

  it("checks scoped identity records, ETags and capture consent fields", () => {
    expect(
      parseIdentityProfile(profile, "service_one").confirmed_at,
    ).toBeUndefined();
    expect(() => parseIdentityProfile(profile, "service_other")).toThrow();
    expect(() =>
      parseIdentityProfile(
        {
          ...profile,
          fingerprint: {
            ...profile.fingerprint,
            headers: { Authorization: "fake-key" },
          },
        },
        "service_one",
      ),
    ).toThrow();
    expect(() =>
      parseIdentityProfileRecord({ profile, etag: "stale" }, "service_one"),
    ).toThrow();
    expect(
      parseIdentityCapture(
        { service_id: "service_one", armed: false, rejected: 0 },
        "service_one",
      ).armed,
    ).toBe(false);
    expect(() =>
      parseIdentityCapture(
        { service_id: "service_one", armed: true, rejected: 0 },
        "service_one",
      ),
    ).toThrow();
    expect(() =>
      parseIdentityProfile(
        { ...profile, confirmed_at: "2020-01-01T00:00:00Z" },
        "service_one",
      ),
    ).toThrow();
  });
});
