// @vitest-environment happy-dom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import {
  armIdentityCapture,
  confirmIdentityProfile,
  createIdentityProfile,
  discardIdentityProfile,
  disarmIdentityCapture,
  getIdentityCapture,
  getIdentityProfile,
  listIdentityProfiles,
} from "./bridge";
import { webInvoke } from "./web-transport";

vi.mock("./edition", () => ({ isWebEdition: true }));
const fetchMock = vi.fn();
const profile = {
  id: "identity_web",
  service_id: "service_web",
  client: "codex_cli",
  source: "builtin",
  fingerprint: {
    user_agent: "codex_cli_rs/0.160.0",
    version: "0.160.0",
    headers: { Originator: "codex_cli_rs" },
  },
  created_at: "2026-10-01T00:00:00Z",
};
const record = { profile, etag: `"sha256:${"a".repeat(64)}"` };
const confirmedETag = `"sha256:${"b".repeat(64)}"`;
const base = "/control/v1/services/service_web";
function respond(value: unknown, status = 200, etag?: string) {
  fetchMock.mockResolvedValueOnce(
    new Response(status === 204 ? null : JSON.stringify(value), {
      status,
      headers: etag ? { ETag: etag } : {},
    }),
  );
}
beforeEach(() => vi.stubGlobal("fetch", fetchMock));
afterEach(() => {
  vi.unstubAllGlobals();
  fetchMock.mockReset();
});

it("connects profile CRUD and confirmation through the real web bridge envelopes", async () => {
  respond({ items: [profile], next_cursor: null });
  expect(await listIdentityProfiles("service_web", "cursor+&")).toEqual({
    items: [profile],
    next_cursor: null,
  });
  expect(fetchMock.mock.calls[0][0]).toBe(
    `${base}/identity-profiles?limit=200&cursor=cursor%2B%26`,
  );
  respond(profile, 200, record.etag);
  const loaded = await getIdentityProfile("service_web", "identity_web");
  expect(loaded).toEqual(record);
  respond(profile, 201, record.etag);
  expect(
    await createIdentityProfile("service_web", "codex_cli", "builtin"),
  ).toEqual(record);
  expect(fetchMock.mock.lastCall).toEqual([
    `${base}/identity-profiles`,
    expect.objectContaining({
      method: "POST",
      body: '{"client":"codex_cli","source":"builtin"}',
    }),
  ]);
  const confirmed = { ...profile, confirmed_at: "2026-10-01T00:01:00Z" };
  respond(confirmed, 200, confirmedETag);
  const reviewed = await confirmIdentityProfile("service_web", loaded);
  expect(reviewed).toEqual({ profile: confirmed, etag: confirmedETag });
  expect(fetchMock.mock.lastCall).toEqual([
    `${base}/identity-profiles/identity_web/confirm`,
    expect.objectContaining({
      method: "POST",
      body: "{}",
      credentials: "same-origin",
      redirect: "error",
      headers: {
        "Content-Type": "application/json",
        "If-Match": record.etag,
        "X-AstrLink-Console": "1",
      },
    }),
  ]);
  respond(null, 204);
  await discardIdentityProfile("service_web", reviewed);
  expect(fetchMock.mock.lastCall).toEqual([
    `${base}/identity-profiles/identity_web`,
    expect.objectContaining({
      method: "DELETE",
      headers: { "If-Match": confirmedETag, "X-AstrLink-Console": "1" },
    }),
  ]);
});

it("connects capture status, scoped arming and disarming without passing local credentials", async () => {
  const status = { service_id: "service_web", armed: false, rejected: 0 };
  respond(status);
  expect(await getIdentityCapture("service_web")).toEqual(status);
  respond({
    ...status,
    armed: true,
    client: "codex_cli",
    expires_at: "2026-10-01T00:10:00Z",
    armed_at: "2026-10-01T00:00:00Z",
  });
  expect((await armIdentityCapture("service_web", "codex_cli")).armed).toBe(
    true,
  );
  expect(fetchMock.mock.lastCall).toEqual([
    `${base}/identity-capture`,
    expect.objectContaining({
      method: "PUT",
      credentials: "same-origin",
      body: '{"client":"codex_cli","ttl_seconds":600}',
      headers: {
        "Content-Type": "application/json",
        "X-AstrLink-Console": "1",
      },
    }),
  ]);
  respond(status);
  expect(await disarmIdentityCapture("service_web")).toEqual(status);
  expect(fetchMock.mock.lastCall?.[1].method).toBe("DELETE");
});

it("requires profile ETags and rejects path injection before fetch", async () => {
  respond(profile);
  await expect(
    getIdentityProfile("service_web", "identity_web"),
  ).rejects.toThrow("ETag");
  fetchMock.mockClear();
  for (const args of [
    { serviceId: "../other", input: { operation: "status" } },
    {
      serviceId: "service_web",
      input: { operation: "get", profile_id: "../other" },
    },
    { serviceId: "service_web", input: { operation: "arbitrary_url" } },
    {
      serviceId: "service_web",
      input: {
        operation: "confirm",
        profile_id: "identity_web",
        etag: "bad\r\n",
      },
    },
  ]) {
    await expect(webInvoke("service_identity", args)).rejects.toThrow();
  }
  expect(fetchMock).not.toHaveBeenCalled();
});
