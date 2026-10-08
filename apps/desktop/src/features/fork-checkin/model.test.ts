import { describe, expect, it } from "vitest";

import {
  CheckinUnsupportedError,
  jobOutcome,
  parseCheckinAccount,
  parseCheckinAccountPage,
  parseCheckinAuthorization,
  parseCheckinError,
  parseCheckinJob,
  parseCheckinJobPage,
  parseCheckinSettings,
  parseCheckinStatus,
  type CheckinJob,
} from "./model";

const fingerprint = "a".repeat(64);
const secret = "FORKCHECKIN-SESSION-PLAINTEXT-MARKER";

const status = {
  protocol_version: 1,
  present: true,
  enabled: true,
  storage_ready: true,
  scheduler_running: true,
};

const account = {
  id: "acct_one",
  dashboard_base_url: "https://relay.example/console",
  state: "connected",
  revision: 2,
  network: { mode: "direct" },
  time_zone: "Asia/Shanghai",
  automatic: true,
  remote_user_id: "7",
  bound_services: ["svc_relay"],
  config_fingerprint: fingerprint,
};

const job = {
  id: "job_one",
  account_id: "acct_one",
  action: "check_in",
  status: "success",
  dispatched: true,
  proof_source: "submission_response",
  site_date: "2026-10-07",
  reward: { known: true, quota: 500000, unit: "quota" },
  created_at: "2026-10-07T01:02:03.456Z",
  finished_at: "2026-10-07T01:02:04Z",
  children: [],
};

describe("check-in status", () => {
  it("accepts the protocol this desktop speaks", () => {
    expect(parseCheckinStatus(status)).toEqual(status);
    expect(
      parseCheckinStatus({
        ...status,
        enabled: false,
        last_error_code: "storage_unavailable",
      }).last_error_code,
    ).toBe("storage_unavailable");
  });

  it("reports another protocol version as unsupported, not malformed", () => {
    expect(() =>
      parseCheckinStatus({ ...status, protocol_version: 2 }),
    ).toThrow(CheckinUnsupportedError);
    expect(() => parseCheckinStatus({ protocol_version: "1" })).toThrow(
      CheckinUnsupportedError,
    );
  });

  it("refuses unknown fields and bad error codes", () => {
    expect(() => parseCheckinStatus({ ...status, token: secret })).toThrow(
      /status\.token/,
    );
    expect(() => parseCheckinStatus({ ...status, present: false })).toThrow();
    expect(() =>
      parseCheckinStatus({ ...status, last_error_code: "Relay said: no" }),
    ).toThrow();
    expect(parseCheckinSettings({ enabled: false })).toEqual({
      enabled: false,
    });
    expect(() => parseCheckinSettings({ enabled: "yes" })).toThrow();
  });
});

describe("check-in accounts", () => {
  it("accepts a connected and a draft account", () => {
    expect(parseCheckinAccount(account)).toEqual(account);
    const draft = {
      ...account,
      state: "draft",
      automatic: false,
      bound_services: [],
    };
    delete (draft as Partial<typeof account>).remote_user_id;
    expect(parseCheckinAccount(draft).state).toBe("draft");
  });

  it("refuses a session-bearing or inconsistent account", () => {
    expect(() => parseCheckinAccount({ ...account, cookie: secret })).toThrow(
      /account\.cookie/,
    );
    expect(() => parseCheckinAccount({ ...account, state: "draft" })).toThrow(
      /remote_user_id/,
    );
    expect(() =>
      parseCheckinAccount({
        ...account,
        state: "auth_required",
        automatic: true,
      }),
    ).toThrow(/automatic/);
    expect(() =>
      parseCheckinAccount({
        ...account,
        bound_services: ["svc_relay", "svc_relay"],
      }),
    ).toThrow(/duplicate/);
    expect(() =>
      parseCheckinAccount({
        ...account,
        dashboard_base_url: "https://user:pw@relay.example",
      }),
    ).toThrow();
    expect(() =>
      parseCheckinAccount({
        ...account,
        network: { mode: "direct", proxy_url: "http://p:1" },
      }),
    ).toThrow(/proxy_url/);
    expect(() =>
      parseCheckinAccount({ ...account, config_fingerprint: "abc" }),
    ).toThrow();
  });

  it("accepts every Core proxy scheme and refuses non-public proxy addresses", () => {
    for (const proxyURL of [
      "http://127.0.0.1:7890",
      "https://proxy.example:443",
      "socks5://127.0.0.1:1080",
      "socks5://[::1]:1080",
    ]) {
      const network = { mode: "custom", proxy_url: proxyURL };
      expect(parseCheckinAccount({ ...account, network }).network).toEqual(
        network,
      );
    }
    for (const proxyURL of [
      "socks5://user:password@proxy.example:1080",
      "socks5://proxy.example:0",
      "http://proxy.example/path",
      "socks5://proxy.example:1080?next=1",
      "socks5://proxy.example:1080#fragment",
      "file:///tmp/proxy",
    ]) {
      expect(() =>
        parseCheckinAccount({
          ...account,
          network: { mode: "custom", proxy_url: proxyURL },
        }),
      ).toThrow(/proxy_url/);
    }
  });

  it("pages with an optional cursor", () => {
    expect(parseCheckinAccountPage({ items: [] })).toEqual({ items: [] });
    expect(
      parseCheckinAccountPage({ items: [account], next_cursor: "c1" })
        .next_cursor,
    ).toBe("c1");
    expect(() => parseCheckinAccountPage({ items: null })).toThrow();
  });
});

describe("check-in jobs", () => {
  it("accepts a proven single job and a batch parent", () => {
    expect(parseCheckinJob(job)).toEqual(job);
    const child = {
      ...job,
      id: "job_child",
      status: "queued",
      dispatched: false,
      proof_source: "none",
    };
    delete (child as Partial<typeof job>).reward;
    delete (child as Partial<typeof job>).finished_at;
    const parent = {
      id: "batch_one",
      action: "check_in",
      status: "queued",
      dispatched: false,
      proof_source: "none",
      created_at: job.created_at,
      children: [child],
    };
    expect(parseCheckinJob(parent).children).toHaveLength(1);
    expect(parseCheckinJobPage({ items: [job, parent] }).items).toHaveLength(2);
  });

  it("accepts successful batches using child proofs rather than a parent proof", () => {
    const parent = {
      id: "batch_done",
      action: "check_in",
      status: "success",
      dispatched: true,
      proof_source: "none",
      created_at: job.created_at,
      finished_at: job.finished_at,
      children: [job],
    };
    expect(parseCheckinJob(parent)).toEqual(parent);
    expect(jobOutcome(parseCheckinJob(parent))).toBe("done");
    expect(() => parseCheckinJob({ ...parent, reward: job.reward })).toThrow(
      /batch/,
    );
    const pending = {
      ...job,
      status: "queued",
      proof_source: "none",
      finished_at: undefined,
    };
    delete pending.finished_at;
    expect(() => parseCheckinJob({ ...parent, children: [pending] })).toThrow(
      /unfinished/,
    );
    expect(() =>
      parseCheckinJob({
        ...parent,
        children: [{ ...job, status: "uncertain", proof_source: "none" }],
      }),
    ).toThrow(/successful children/);
    expect(() =>
      parseCheckinJob({
        ...job,
        status: "already_checked",
        proof_source: "none",
      }),
    ).toThrow(/without proof/);

    const orphan: Partial<typeof job> = { ...job };
    delete orphan.account_id;
    expect(() => parseCheckinJob({ ...parent, children: [orphan] })).toThrow(
      /account_id/,
    );
  });

  it("never accepts success without proof or an estimated reward", () => {
    expect(() => parseCheckinJob({ ...job, proof_source: "none" })).toThrow(
      /without proof/,
    );
    expect(() => parseCheckinJob({ ...job, dispatched: false })).toThrow(
      /without dispatch/,
    );
    expect(() =>
      parseCheckinJob({ ...job, reward: { known: false, quota: 1 } }),
    ).toThrow();
    expect(() =>
      parseCheckinJob({
        ...job,
        reward: { known: true, quota: -1, unit: "quota" },
      }),
    ).toThrow();
    expect(() => parseCheckinJob({ ...job, status: "pending" })).toThrow(
      /status/,
    );
    expect(() => parseCheckinJob({ ...job, status: "running" })).toThrow(
      /finished_at/,
    );
  });

  it("refuses nested batches and orphan jobs", () => {
    const nested = {
      ...job,
      id: "batch_two",
      children: [{ ...job, children: [job] }],
    };
    delete (nested as Partial<typeof job>).account_id;
    expect(() => parseCheckinJob(nested)).toThrow(/children/);
    const orphan = { ...job };
    delete (orphan as Partial<typeof job>).account_id;
    expect(() => parseCheckinJob(orphan)).toThrow(/account_id/);
    expect(() => parseCheckinJob({ ...job, message: secret })).toThrow(
      /job\.message/,
    );
  });

  it("classifies outcomes without trusting HTTP success", () => {
    const outcome = (fields: Partial<CheckinJob>) =>
      jobOutcome({ ...(job as CheckinJob), ...fields });
    expect(outcome({})).toBe("done");
    expect(outcome({ status: "already_checked" })).toBe("done");
    expect(outcome({ status: "running" })).toBe("pending");
    expect(outcome({ status: "manual_required" })).toBe("needs_operator");
    expect(outcome({ status: "uncertain" })).toBe("uncertain");
    expect(outcome({ status: "cancelled", dispatched: true })).toBe(
      "uncertain",
    );
    expect(outcome({ status: "cancelled", dispatched: false })).toBe(
      "not_done",
    );
    expect(outcome({ status: "not_checked" })).toBe("not_done");
    expect(outcome({ status: "rate_limited" })).toBe("failed");
  });
});

describe("check-in authorization and errors", () => {
  const authorization = {
    session_id: "sess_0123456789",
    account_id: "acct_one",
    expires_at: "2026-10-07T01:12:03Z",
    config_fingerprint: fingerprint,
    login_url: "https://relay.example/login",
  };

  it("accepts a login session and refuses one carrying a credential", () => {
    expect(parseCheckinAuthorization(authorization)).toEqual(authorization);
    expect(() =>
      parseCheckinAuthorization({ ...authorization, credential: secret }),
    ).toThrow();
    expect(() =>
      parseCheckinAuthorization({
        ...authorization,
        login_url: "file:///etc/passwd",
      }),
    ).toThrow();
    expect(() =>
      parseCheckinAuthorization({
        ...authorization,
        login_url: "https://relay.example/?next=x",
      }),
    ).toThrow();
  });

  it("reads the sanitized error envelope", () => {
    expect(
      parseCheckinError({
        error: {
          code: "job_not_cancellable",
          message: "finished",
          retryable: false,
          details: [],
        },
        request_id: "req_1",
      }),
    ).toEqual({
      code: "job_not_cancellable",
      message: "finished",
      retryable: false,
    });
    expect(() =>
      parseCheckinError({
        error: { code: "Bad Code", message: "x", retryable: false },
      }),
    ).toThrow();
  });

  it("does not echo rejected values in its messages", () => {
    let message = "";
    try {
      parseCheckinAccount({ ...account, remote_user_id: secret.repeat(10) });
    } catch (error) {
      message = (error as Error).message;
    }
    expect(message).toContain("remote_user_id");
    expect(message).not.toContain(secret);
  });
});
