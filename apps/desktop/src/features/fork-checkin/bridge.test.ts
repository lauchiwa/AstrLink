import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const invokeMock = vi.hoisted(() => vi.fn());

vi.mock("@tauri-apps/api/core", () => ({
  invoke: invokeMock,
}));

import {
  CheckinBridgeError,
  cancelCheckinJob,
  createCheckinJob,
  deleteCheckinAccount,
  describeCheckinError,
  completeCheckinAuthorization,
  listCheckinJobs,
  loadCheckinAvailability,
  updateCheckinAccount,
  updateCheckinSettings,
} from "./bridge";

const secret = "FORKCHECKIN-SESSION-PLAINTEXT-MARKER";
const status = {
  protocol_version: 1,
  present: true,
  enabled: false,
  storage_ready: false,
  scheduler_running: false,
};
const connected = {
  id: "acct_one",
  dashboard_base_url: "https://relay.example",
  state: "connected",
  revision: 3,
  network: { mode: "direct" },
  time_zone: "Asia/Shanghai",
  automatic: false,
  remote_user_id: "7",
  bound_services: [],
  config_fingerprint: "a".repeat(64),
};
const job = {
  id: "job_one",
  account_id: "acct_one",
  action: "check_in",
  status: "queued",
  dispatched: false,
  proof_source: "none",
  created_at: "2026-01-02T03:04:05Z",
  children: [],
};

/** The native result, as `operation::OperationResult` serializes it. */
function native(
  outcome: string,
  {
    body = null,
    code = null,
    requestID = null,
    retryable = false,
    status = null,
  }: {
    body?: unknown;
    code?: string | null;
    requestID?: string | null;
    retryable?: boolean;
    status?: number | null;
  } = {},
) {
  return {
    outcome,
    http_status: status,
    request_id: requestID,
    body,
    error_code: code,
    retryable,
  };
}

function accepted(status: number, body: unknown = null, requestID?: string) {
  return native("accepted", { body, requestID, status });
}

// Core's envelope rides along in `body`; its message must never surface.
function refusal(
  outcome: string,
  status: number,
  code: string,
  {
    requestID,
    retryable = false,
  }: { requestID?: string; retryable?: boolean } = {},
) {
  return native(outcome, {
    body: {
      error: { code, message: secret, retryable, details: [] },
      request_id: "req_core_1",
    },
    code,
    requestID,
    retryable,
    status,
  });
}

async function failure(promise: Promise<unknown>): Promise<CheckinBridgeError> {
  const error = await promise.then(
    () => null,
    (reason: unknown) => reason,
  );
  expect(error).toBeInstanceOf(CheckinBridgeError);
  return error as CheckinBridgeError;
}

describe("check-in bridge", () => {
  beforeEach(() => {
    vi.stubGlobal("window", { __TAURI_INTERNALS__: {} });
    invokeMock.mockReset();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("reports desktop-only without invoking outside Tauri", async () => {
    vi.stubGlobal("window", {});

    await expect(loadCheckinAvailability()).resolves.toEqual({
      kind: "desktop_only",
    });
    expect((await failure(listCheckinJobs())).kind).toBe("desktop_only");
    expect(invokeMock).not.toHaveBeenCalled();
  });

  it("sends one fixed command with a closed operation", async () => {
    invokeMock.mockResolvedValueOnce(accepted(200, status));

    await expect(loadCheckinAvailability()).resolves.toEqual({
      kind: "present",
      status,
    });
    expect(invokeMock).toHaveBeenCalledWith("fork_checkin_operation", {
      operation: { op: "status" },
    });
  });

  it("tells a Core without the extension from a different protocol", async () => {
    invokeMock.mockResolvedValueOnce(
      native("extension_unavailable", { status: 404 }),
    );
    await expect(loadCheckinAvailability()).resolves.toEqual({
      kind: "unavailable",
    });

    invokeMock.mockResolvedValueOnce(refusal("not_found", 404, "not_found"));
    await expect(loadCheckinAvailability()).resolves.toEqual({
      kind: "unavailable",
    });

    invokeMock.mockResolvedValueOnce(
      native("extension_unsupported", {
        body: { ...status, protocol_version: 2 },
        status: 200,
      }),
    );
    await expect(loadCheckinAvailability()).resolves.toEqual({
      kind: "unsupported",
    });

    // A body that slips past the native version check is still refused.
    invokeMock.mockResolvedValueOnce(
      accepted(200, { ...status, protocol_version: 2, extra: secret }),
    );
    await expect(loadCheckinAvailability()).resolves.toEqual({
      kind: "unsupported",
    });
  });

  it("rejects malformed native results and bodies without echoing them", async () => {
    for (const raw of [
      null,
      "ok",
      { ...accepted(200, status), extra: secret },
      accepted(99, status),
      accepted(404, status),
      native("accepted", { body: status }),
      accepted(200, status, "short"),
      native("transport_failed", { requestID: `${secret}!`, retryable: true }),
      native("conflict", { code: "Not-A-Code", status: 409 }),
      { ...native("something_else"), body: secret },
      { outcome: "accepted", http_status: 200, body: status },
      native("unexpected", { body: secret, status: 200 }),
    ]) {
      invokeMock.mockResolvedValueOnce(raw);
      const error = await failure(loadCheckinAvailability());
      expect(error.kind).toBe("malformed");
      expect(error.message).not.toContain(secret);
    }

    invokeMock.mockResolvedValueOnce(
      accepted(200, { ...status, session: secret }),
    );
    const error = await failure(loadCheckinAvailability());
    expect(error.kind).toBe("malformed");
    expect(error.message).not.toContain(secret);
    expect(describeCheckinError(error)).not.toContain(secret);
  });

  it("requires all six native result fields even when their values are null", async () => {
    const complete: Record<string, unknown> = accepted(200, status);
    for (const key of Object.keys(complete)) {
      const missing = { ...complete };
      delete missing[key];
      invokeMock.mockResolvedValueOnce(missing);
      expect((await failure(loadCheckinAvailability())).kind).toBe("malformed");
    }
    for (const key of ["http_status", "request_id", "error_code"]) {
      invokeMock.mockResolvedValueOnce({ ...complete, [key]: undefined });
      expect((await failure(loadCheckinAvailability())).kind).toBe("malformed");
    }
  });

  it("keeps a submitted resume id when a retry has no trustworthy answer", async () => {
    invokeMock.mockResolvedValueOnce(
      native("transport_failed", {
        requestID: "req_original",
        retryable: true,
      }),
    );
    const input = { action: "check_in" as const, accounts: ["acct_one"] };
    const lost = await failure(createCheckinJob(input));
    expect(lost.resume?.requestID).toBe("req_original");

    for (const reply of [
      null,
      accepted(202, job),
      accepted(202, job, "req_different"),
    ]) {
      invokeMock.mockResolvedValueOnce(reply);
      const retry = await failure(createCheckinJob(input, lost.resume));
      expect(retry.kind).toBe("malformed");
      expect(retry.resume).toEqual(lost.resume);
    }
    invokeMock.mockRejectedValueOnce(`unusable reply: ${secret}`);
    const refused = await failure(createCheckinJob(input, lost.resume));
    expect(refused.kind).toBe("refused");
    expect(refused.resume).toEqual(lost.resume);
    expect(refused.message).not.toContain(secret);

    invokeMock.mockResolvedValueOnce(null);
    const changed = await failure(
      createCheckinJob({ ...input, action: "status_refresh" }, lost.resume),
    );
    expect(changed.resume).toBeNull();
  });

  it("rejects successful write replies without a native request id", async () => {
    invokeMock.mockResolvedValueOnce(accepted(202, job));
    expect(
      (
        await failure(
          createCheckinJob({ action: "check_in", accounts: ["acct_one"] }),
        )
      ).kind,
    ).toBe("malformed");
  });

  it("maps a local refusal without its native reason", async () => {
    invokeMock.mockRejectedValueOnce(`window refused: ${secret}`);

    const error = await failure(listCheckinJobs());

    expect(error.kind).toBe("refused");
    expect(error.message).not.toContain(secret);
    expect(describeCheckinError(error)).not.toContain(secret);
  });

  it("returns a resume handle after a lost response and replays the same id", async () => {
    invokeMock.mockResolvedValueOnce(
      native("transport_failed", {
        requestID: "req_native_1",
        retryable: true,
      }),
    );

    const lost = await failure(
      createCheckinJob({ action: "check_in", accounts: ["acct_one"] }),
    );

    expect(lost.kind).toBe("transport");
    expect(lost.retryable).toBe(true);
    expect(lost.resume?.requestID).toBe("req_native_1");
    expect(invokeMock).toHaveBeenLastCalledWith("fork_checkin_operation", {
      operation: {
        op: "create_job",
        action: "check_in",
        accounts: ["acct_one"],
      },
    });

    invokeMock.mockResolvedValueOnce(accepted(202, job, "req_native_1"));
    await expect(
      createCheckinJob(
        { accounts: ["acct_one"], action: "check_in" },
        lost.resume,
      ),
    ).resolves.toEqual(job);
    expect(invokeMock).toHaveBeenLastCalledWith("fork_checkin_operation", {
      operation: {
        op: "create_job",
        action: "check_in",
        accounts: ["acct_one"],
        request_id: "req_native_1",
      },
    });
  });

  it("never reuses an id for different content or another operation", async () => {
    invokeMock.mockResolvedValueOnce(
      native("transport_failed", {
        requestID: "req_native_2",
        retryable: true,
      }),
    );
    const lost = await failure(
      createCheckinJob({ action: "check_in", accounts: ["acct_one"] }),
    );

    invokeMock.mockResolvedValue(accepted(202, job, "req_native_3"));
    await createCheckinJob(
      { action: "status_refresh", accounts: ["acct_one"] },
      lost.resume,
    );
    await cancelCheckinJob("job_one", lost.resume);

    for (const [, payload] of invokeMock.mock.calls.slice(1)) {
      expect(
        (payload as { operation: Record<string, unknown> }).operation,
      ).not.toHaveProperty("request_id");
    }
  });

  it("keeps a resume handle only for retryable refusals", async () => {
    invokeMock.mockResolvedValueOnce(
      refusal("temporarily_unavailable", 503, "checkin_stopping", {
        requestID: "req_native_4",
        retryable: true,
      }),
    );
    const stopping = await failure(updateCheckinSettings(true));
    expect(stopping.kind).toBe("rejected");
    expect(stopping.code).toBe("checkin_stopping");
    expect(stopping.resume?.requestID).toBe("req_native_4");

    invokeMock.mockResolvedValueOnce(
      refusal("revision_conflict", 412, "revision_conflict", {
        requestID: "req_native_5",
      }),
    );
    const conflict = await failure(
      updateCheckinAccount("acct_one", {
        expected_revision: 2,
        automatic: true,
      }),
    );
    expect(conflict.code).toBe("revision_conflict");
    expect(conflict.httpStatus).toBe(412);
    expect(conflict.resume).toBeNull();
  });

  it("treats a Core that is not ready like a lost answer", async () => {
    invokeMock.mockResolvedValueOnce(
      native("core_not_ready", { requestID: "req_native_8", retryable: true }),
    );

    const error = await failure(cancelCheckinJob("job_one"));

    expect(error.kind).toBe("transport");
    expect(error.resume?.requestID).toBe("req_native_8");
  });

  it("treats only an empty 204 as a completed delete", async () => {
    invokeMock.mockResolvedValueOnce(accepted(204, null, "req_native_6"));
    await expect(deleteCheckinAccount("acct_one", 3)).resolves.toBeUndefined();
    expect(invokeMock).toHaveBeenLastCalledWith("fork_checkin_operation", {
      operation: {
        op: "delete_account",
        account_id: "acct_one",
        expected_revision: 3,
      },
    });

    invokeMock.mockResolvedValueOnce(accepted(200, { deleted: true }));
    expect((await failure(deleteCheckinAccount("acct_one", 3))).kind).toBe(
      "malformed",
    );
  });

  it("does not read HTTP 2xx as a finished check-in", async () => {
    invokeMock.mockResolvedValueOnce(
      accepted(202, { ...job, status: "success" }, "req_native_7"),
    );

    const error = await failure(
      createCheckinJob({ action: "check_in", accounts: ["acct_one"] }),
    );

    // A success without proof is not a receipt this build accepts.
    expect(error.kind).toBe("malformed");
  });

  it("keeps the pasted session out of the resume handle it hands back", async () => {
    const completion = {
      session_id: "session_0001",
      dashboard_base_url: "https://relay.example",
      expected_revision: 3,
    };
    // A lost answer must stay retryable under the id Core already issued.
    invokeMock.mockResolvedValueOnce(
      native("transport_failed", { requestID: "req_native_one" }),
    );
    const lost = await failure(
      completeCheckinAuthorization({ ...completion, pasted_cookies: secret }),
    );
    expect(lost.kind).toBe("transport");
    expect(lost.resume?.requestID).toBe("req_native_one");
    // Core excludes the captured session from its own completion
    // fingerprint; so must this, or the credential would live on in a handle
    // the dialog keeps after clearing its textarea.
    expect(JSON.stringify(lost.resume)).not.toContain(secret);
    expect(lost.message).not.toContain(secret);

    // Pasting a different session is the same logical action, so the id is
    // reused rather than silently replaced.
    invokeMock.mockResolvedValueOnce(
      accepted(200, connected, "req_native_one"),
    );
    await expect(
      completeCheckinAuthorization(
        { ...completion, pasted_cookies: `${secret}-other` },
        lost.resume,
      ),
    ).resolves.toMatchObject({ state: "connected", remote_user_id: "7" });
    expect(invokeMock).toHaveBeenLastCalledWith("fork_checkin_operation", {
      operation: {
        op: "complete_authorization",
        ...completion,
        pasted_cookies: `${secret}-other`,
        request_id: "req_native_one",
      },
    });
  });

  it("will not replay a completion Core refused outright", async () => {
    invokeMock.mockResolvedValueOnce(
      refusal("conflict", 409, "identity_mismatch"),
    );
    const refused = await failure(
      completeCheckinAuthorization({
        session_id: "session_0001",
        pasted_cookies: secret,
        dashboard_base_url: "https://relay.example",
      }),
    );
    // Nothing was written, and the id must not be resent under this content.
    expect(refused.code).toBe("identity_mismatch");
    expect(refused.retryable).toBe(false);
    expect(refused.resume).toBeNull();
    expect(describeCheckinError(refused)).not.toContain(secret);
  });

  it("describes Core codes with local copy and never the envelope text", async () => {
    invokeMock.mockResolvedValueOnce(
      refusal("conflict", 409, "job_not_cancellable"),
    );
    const known = await failure(cancelCheckinJob("job_one"));
    invokeMock.mockResolvedValueOnce(
      refusal("conflict", 409, "brand_new_code"),
    );
    const unknown = await failure(cancelCheckinJob("job_one"));
    // A refusal without a well-formed code falls back to its outcome.
    invokeMock.mockResolvedValueOnce(
      native("forbidden", { body: { secret }, status: 403 }),
    );
    const forbidden = await failure(cancelCheckinJob("job_one"));

    expect(describeCheckinError(known)).toBe("任务已结束或已发送，无法取消。");
    expect(describeCheckinError(unknown)).toBe(
      "网关拒绝了该请求（brand_new_code）。",
    );
    expect(forbidden.code).toBe("forbidden");
    expect(describeCheckinError(forbidden)).toBe(
      "网关拒绝了该请求（forbidden）。",
    );
    for (const error of [known, unknown, forbidden]) {
      expect(error.message).not.toContain(secret);
      expect(describeCheckinError(error)).not.toContain(secret);
    }
    expect(describeCheckinError(new Error(secret))).not.toContain(secret);
  });
});
