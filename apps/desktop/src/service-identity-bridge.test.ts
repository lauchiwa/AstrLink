import { afterEach, beforeEach, expect, it, vi } from "vitest";
const invoke = vi.hoisted(() => vi.fn());
vi.mock("@tauri-apps/api/core", () => ({ invoke }));
import {
  armIdentityCapture,
  confirmIdentityProfile,
  createIdentityProfile,
  discardIdentityProfile,
  getIdentityProfile,
  listIdentityProfiles,
} from "./bridge";
import type { IdentityProfileRecord } from "./request-compatibility-model";
const record: IdentityProfileRecord = {
  profile: {
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
  },
  etag: `"sha256:${"a".repeat(64)}"`,
};
beforeEach(() => {
  vi.resetAllMocks();
  vi.stubGlobal("window", { __TAURI_INTERNALS__: {} });
});
afterEach(() => vi.unstubAllGlobals());
it("carries scope, pagination, and the reviewed ETag through the closed native command", async () => {
  invoke.mockResolvedValueOnce({
    items: [record.profile],
    next_cursor: "next",
  });
  expect(
    (await listIdentityProfiles("service_one", "cursor")).next_cursor,
  ).toBe("next");
  expect(invoke).toHaveBeenLastCalledWith("service_identity", {
    serviceId: "service_one",
    input: { operation: "list", cursor: "cursor" },
  });
  invoke.mockResolvedValueOnce(record);
  expect(await getIdentityProfile("service_one", "identity_one")).toEqual(
    record,
  );
  invoke.mockResolvedValueOnce(record);
  await createIdentityProfile(
    "service_one",
    "codex_cli",
    "subscription_import",
  );
  expect(invoke).toHaveBeenLastCalledWith("service_identity", {
    serviceId: "service_one",
    input: {
      operation: "create",
      client: "codex_cli",
      source: "subscription_import",
    },
  });
  invoke.mockResolvedValueOnce({
    ...record,
    profile: { ...record.profile, confirmed_at: "2026-08-01T00:01:00Z" },
  });
  await confirmIdentityProfile("service_one", record);
  expect(invoke).toHaveBeenLastCalledWith("service_identity", {
    serviceId: "service_one",
    input: {
      operation: "confirm",
      profile_id: "identity_one",
      etag: record.etag,
    },
  });
  invoke.mockResolvedValueOnce(null);
  await discardIdentityProfile("service_one", record);
  expect(invoke).toHaveBeenLastCalledWith("service_identity", {
    serviceId: "service_one",
    input: {
      operation: "discard",
      profile_id: "identity_one",
      etag: record.etag,
    },
  });
});
it("rejects cross-service responses and propagates native failures", async () => {
  invoke.mockResolvedValueOnce(record);
  await expect(
    getIdentityProfile("service_other", "identity_one"),
  ).rejects.toThrow("identity.service_id");
  invoke.mockResolvedValueOnce(record);
  await expect(
    getIdentityProfile("service_one", "identity_other"),
  ).rejects.toThrow("ID mismatch");
  invoke.mockRejectedValueOnce("If-Match failed");
  await expect(confirmIdentityProfile("service_one", record)).rejects.toThrow(
    "If-Match failed",
  );
});
it("requires a native bridge and only sends the selected client when arming capture", async () => {
  invoke.mockResolvedValueOnce({
    service_id: "service_one",
    armed: true,
    client: "codex_cli",
    armed_at: "2026-08-01T00:00:00Z",
    expires_at: "2026-08-01T00:10:00Z",
    rejected: 0,
  });
  await armIdentityCapture("service_one", "codex_cli");
  expect(invoke).toHaveBeenCalledWith("service_identity", {
    serviceId: "service_one",
    input: { operation: "arm", client: "codex_cli" },
  });
  vi.stubGlobal("window", {});
  await expect(
    armIdentityCapture("service_one", "codex_cli"),
  ).rejects.toThrow();
  expect(invoke).toHaveBeenCalledTimes(1);
});
