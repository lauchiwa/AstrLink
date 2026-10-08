// @vitest-environment happy-dom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const bridge = vi.hoisted(() => ({
  createCheckinJob: vi.fn(),
  cancelCheckinJob: vi.fn(),
  getCheckinJob: vi.fn(),
}));
vi.mock("./bridge", async (original) => ({
  ...(await original<typeof import("./bridge")>()),
  ...bridge,
}));
import { CheckinBridgeError } from "./bridge";
import type { CheckinAccount, CheckinJob, JobStatus } from "./model";
import {
  BATCH_SLOT,
  MAX_POLL_REQUESTS,
  POLL_INTERVAL_MS,
  readSlot,
  useCheckinActions,
  type CheckinActions,
} from "./use-checkin-actions";

const account: CheckinAccount = {
  id: "acct_one",
  state: "connected",
  revision: 2,
  dashboard_base_url: "https://relay.example",
  time_zone: "UTC",
  network: { mode: "direct" },
  automatic: false,
  remote_user_id: "7",
  bound_services: [],
  config_fingerprint: "a".repeat(64),
};
function job(
  status: JobStatus = "queued",
  id = "job_one",
  accountID = account.id,
): CheckinJob {
  const terminal = status !== "queued" && status !== "running";
  return {
    id,
    account_id: accountID,
    action: "check_in",
    status,
    dispatched: status === "success",
    proof_source: status === "success" ? "submission_response" : "none",
    created_at: new Date(0).toISOString(),
    ...(terminal ? { finished_at: new Date(1000).toISOString() } : {}),
    children: [],
  };
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((yes, no) => {
    resolve = yes;
    reject = no;
  });
  return { promise, resolve, reject };
}
const resume = {
  op: "create_job" as const,
  fingerprint: "fixed-payload",
  requestID: "req_original",
};

describe("check-in action lifecycle", () => {
  let root: Root;
  let container: HTMLDivElement;
  let actions: CheckinActions;
  let props: Parameters<typeof useCheckinActions>[0];
  const onSettled = vi.fn();
  function Harness() {
    actions = useCheckinActions(props);
    return null;
  }
  beforeEach(() => {
    (
      globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT?: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = true;
    vi.resetAllMocks();
    vi.useFakeTimers();
    props = {
      active: true,
      accounts: [account],
      sessionKey: "core-one",
      onSettled,
    };
    bridge.createCheckinJob.mockResolvedValue(job());
    bridge.getCheckinJob.mockImplementation(async (id: string) =>
      job("queued", id),
    );
    container = document.createElement("div");
    document.body.append(container);
    root = createRoot(container);
  });
  afterEach(async () => {
    await act(async () => root.unmount());
    container.remove();
    vi.useRealTimers();
  });
  const render = async (patch: Partial<typeof props> = {}) => {
    props = { ...props, ...patch };
    await act(async () => root.render(<Harness />));
  };
  const tick = async (count = 1) => {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * count);
    });
  };

  it("does not send writes or poll when inactive", async () => {
    await render({ active: false });
    await act(async () => {
      actions.adopt([job()]);
      actions.run("check_in", [account.id]);
      actions.cancel(job());
      actions.retry(account.id);
    });
    await tick(4);
    expect(bridge.createCheckinJob).not.toHaveBeenCalled();
    expect(bridge.cancelCheckinJob).not.toHaveBeenCalled();
    expect(bridge.getCheckinJob).not.toHaveBeenCalled();
  });

  it("serializes duplicate clicks, pins one revision, and polls only local receipts until settled", async () => {
    const pending = deferred<CheckinJob>();
    bridge.createCheckinJob.mockReturnValue(pending.promise);
    await render();
    await act(async () => {
      expect(actions.run("check_in", [account.id])).toBe(true);
      expect(actions.run("check_in", [account.id])).toBe(false);
    });
    expect(bridge.createCheckinJob).toHaveBeenCalledOnce();
    expect(bridge.createCheckinJob).toHaveBeenCalledWith(
      { action: "check_in", accounts: [account.id], expected_revision: 2 },
      null,
    );
    await act(async () => pending.resolve(job()));
    bridge.getCheckinJob.mockResolvedValue(job("success"));
    await tick();
    expect(actions.latest(account.id)?.status).toBe("success");
    expect(onSettled).toHaveBeenCalledWith([account.id]);
    await tick(5);
    expect(bridge.getCheckinJob).toHaveBeenCalledOnce();
    expect(bridge.createCheckinJob).toHaveBeenCalledOnce();
  });

  it("bounds and normalizes batch selection without sending a batch revision", async () => {
    const accounts = Array.from({ length: 26 }, (_, index) => ({
      ...account,
      id: `acct_${index}`,
    }));
    await render({ accounts });
    await act(async () => {
      expect(actions.run("check_in", [])).toBe(false);
      expect(
        actions.run(
          "check_in",
          accounts.map((value) => value.id),
        ),
      ).toBe(false);
      expect(
        actions.run(
          "check_in",
          accounts
            .slice(0, 25)
            .reverse()
            .map((value) => value.id),
        ),
      ).toBe(true);
    });
    expect(bridge.createCheckinJob).toHaveBeenCalledWith(
      {
        action: "check_in",
        accounts: accounts
          .slice(0, 25)
          .map((value) => value.id)
          .sort(),
      },
      null,
    );
  });

  it("offers only status reads for manual or dispatched uncertain accounts", async () => {
    await render({
      accounts: [
        account,
        { ...account, id: "acct_manual", state: "manual_required" },
        { ...account, id: "acct_draft", state: "draft" },
      ],
    });
    await act(async () =>
      actions.adopt([{ ...job("uncertain"), dispatched: true }]),
    );
    expect(actions.canRun("check_in", account.id)).toBe(false);
    expect(actions.canRun("status_refresh", account.id)).toBe(true);
    await act(async () => {
      expect(actions.run("check_in", [account.id])).toBe(false);
      expect(actions.run("check_in", ["acct_manual"])).toBe(false);
      expect(actions.run("status_refresh", ["acct_draft"])).toBe(false);
      expect(actions.run("status_refresh", [account.id])).toBe(true);
    });
    expect(bridge.createCheckinJob).toHaveBeenCalledTimes(1);
    expect(bridge.createCheckinJob.mock.calls[0][0].action).toBe(
      "status_refresh",
    );
  });

  it("retries a lost receipt with its original payload and id, even after the account revision changes", async () => {
    bridge.createCheckinJob.mockRejectedValueOnce(
      new CheckinBridgeError("transport", { resume, retryable: true }),
    );
    await render();
    await act(async () => {
      actions.run("check_in", [account.id]);
    });
    expect(actions.message(account.id)?.retry).toBe(true);
    await render({ accounts: [{ ...account, revision: 9 }] });
    await act(async () => actions.retry(account.id));
    expect(bridge.createCheckinJob).toHaveBeenLastCalledWith(
      { action: "check_in", accounts: [account.id], expected_revision: 2 },
      resume,
    );
    expect(actions.message(account.id)).toBeUndefined();
  });

  it.each(["pause", "unmount", "core-aba"] as const)(
    "drops late poll responses and callbacks after %s",
    async (change) => {
      const pending = deferred<CheckinJob>();
      bridge.getCheckinJob.mockReturnValue(pending.promise);
      await render();
      await act(async () => actions.adopt([job()]));
      await tick();
      if (change === "pause") await render({ active: false });
      else if (change === "unmount") {
        await act(async () => root.unmount());
        root = createRoot(container);
      } else {
        await render({ sessionKey: "core-two" });
        await render({ sessionKey: "core-one", active: false });
      }
      await act(async () => pending.resolve(job("success")));
      await tick(4);
      expect(onSettled).not.toHaveBeenCalled();
      expect(bridge.getCheckinJob).toHaveBeenCalledOnce();
      expect(bridge.createCheckinJob).not.toHaveBeenCalled();
    },
  );

  it("does not let an old Core ABA write overwrite a newer retry handle", async () => {
    const old = deferred<CheckinJob>();
    const newer = { ...resume, requestID: "req_newer" };
    bridge.createCheckinJob
      .mockReturnValueOnce(old.promise)
      .mockRejectedValueOnce(
        new CheckinBridgeError("transport", { resume: newer }),
      );
    await render();
    await act(async () => {
      actions.run("check_in", [account.id]);
    });
    await render({ sessionKey: "core-two" });
    await render({ sessionKey: "core-one" });
    await act(async () => {
      actions.run("check_in", [account.id]);
    });
    await act(async () =>
      old.reject(new CheckinBridgeError("transport", { resume })),
    );
    await act(async () => actions.retry(account.id));
    expect(bridge.createCheckinJob).toHaveBeenLastCalledWith(
      { action: "check_in", accounts: [account.id], expected_revision: 2 },
      newer,
    );
  });

  it.each(["success", "failure"] as const)(
    "ignores a late write %s after going offline",
    async (outcome) => {
      const pending = deferred<CheckinJob>();
      bridge.createCheckinJob.mockReturnValue(pending.promise);
      await render();
      await act(async () => {
        actions.run("check_in", [account.id]);
      });
      await render({ active: false });
      await act(async () => {
        if (outcome === "success") pending.resolve(job("success"));
        else pending.reject(new CheckinBridgeError("transport", { resume }));
      });
      expect(onSettled).not.toHaveBeenCalled();
      expect(actions.message(account.id)).toBeUndefined();
      await tick(4);
      expect(bridge.getCheckinJob).not.toHaveBeenCalled();
    },
  );

  it("does not let a poll started before cancellation undo its terminal receipt", async () => {
    const poll = deferred<CheckinJob>();
    bridge.getCheckinJob.mockReturnValue(poll.promise);
    bridge.cancelCheckinJob.mockResolvedValue(job("cancelled"));
    await render();
    await act(async () => actions.adopt([job()]));
    await tick();
    await act(async () => {
      actions.cancel(job());
      actions.cancel(job());
    });
    expect(bridge.cancelCheckinJob).toHaveBeenCalledOnce();
    await act(async () => poll.resolve(job()));
    expect(actions.latest(account.id)?.status).toBe("cancelled");
    expect(onSettled).toHaveBeenCalledTimes(1);
    await tick(3);
    expect(bridge.getCheckinJob).toHaveBeenCalledOnce();
  });

  it("preserves dispatch and running progress against stale list reads", async () => {
    await render();
    await act(async () =>
      actions.adopt([{ ...job("running"), dispatched: true }]),
    );
    await act(async () => actions.adopt([job()]));
    expect(actions.latest(account.id)?.dispatched).toBe(true);
    expect(actions.latest(account.id)?.status).toBe("running");
    await act(async () => actions.cancel(job()));
    expect(bridge.cancelCheckinJob).not.toHaveBeenCalled();
  });

  it("reports a dispatch race without claiming that cancelling undid the submission", async () => {
    bridge.cancelCheckinJob.mockResolvedValue({
      ...job("running"),
      dispatched: true,
    });
    await render();
    await act(async () => actions.adopt([job()]));
    await act(async () => actions.cancel(job()));
    expect(actions.message(account.id)?.text).toContain("取消不会撤回");
    expect(actions.latest(account.id)?.status).toBe("running");
    expect(actions.latest(account.id)?.dispatched).toBe(true);
  });

  it("adopts pending batches as one polling unit without inferring success for failed children", async () => {
    const second = { ...account, id: "acct_two" };
    const batch: CheckinJob = {
      ...job("running", "batch_one"),
      account_id: undefined,
      children: [job(), job("auth_required", "job_two", second.id)],
    };
    await render({ accounts: [account, second] });
    await act(async () => actions.adopt([batch]));
    expect(onSettled).not.toHaveBeenCalled();
    bridge.getCheckinJob.mockResolvedValue({
      ...batch,
      status: "auth_required",
      children: [job("success"), batch.children[1]],
    });
    await tick();
    expect(bridge.getCheckinJob).toHaveBeenCalledWith(batch.id);
    expect(bridge.getCheckinJob).toHaveBeenCalledOnce();
    expect(actions.latest(account.id)?.status).toBe("success");
    expect(actions.latest(second.id)?.status).toBe("auth_required");
    expect(actions.latestBatch?.status).toBe("auth_required");
    expect(actions.records).toHaveLength(1);
    expect(onSettled).toHaveBeenCalledWith([account.id]);
  });

  it("retries batch creation from its message without reconstructing a lost selection", async () => {
    const second = { ...account, id: "acct_two" };
    bridge.createCheckinJob.mockRejectedValueOnce(
      new CheckinBridgeError("transport", { resume }),
    );
    await render({ accounts: [account, second] });
    await act(async () => {
      actions.run("check_in", [second.id, account.id]);
    });
    expect(actions.message(BATCH_SLOT)?.retry).toBe(true);
    await act(async () => actions.retry(BATCH_SLOT));
    expect(bridge.createCheckinJob).toHaveBeenLastCalledWith(
      { action: "check_in", accounts: [account.id, second.id] },
      resume,
    );
  });

  it("shows bounded polling errors, rejects mismatched ids, and never resubmits a job", async () => {
    bridge.getCheckinJob
      .mockRejectedValueOnce(new Error("untrusted response text"))
      .mockResolvedValueOnce(job("success", "job_other"))
      .mockResolvedValueOnce(job("success"));
    await render();
    await act(async () => actions.adopt([job()]));
    await tick();
    expect(actions.message(readSlot("job_one"))?.text).not.toContain(
      "untrusted",
    );
    await tick();
    expect(actions.receipt("job_other")).toBeUndefined();
    expect(actions.message(readSlot("job_one"))).toBeDefined();
    await tick();
    expect(actions.message(readSlot("job_one"))).toBeUndefined();
    expect(bridge.createCheckinJob).not.toHaveBeenCalled();
  });

  it("forgets a missing receipt without repeatedly polling it", async () => {
    bridge.getCheckinJob.mockRejectedValue(
      new CheckinBridgeError("rejected", { code: "not_found" }),
    );
    await render();
    await act(async () => actions.adopt([job()]));
    await tick(3);
    expect(actions.forgotten("job_one")).toBe(true);
    expect(actions.records).toHaveLength(0);
    expect(bridge.getCheckinJob).toHaveBeenCalledOnce();
  });

  it("bounds each poll round and rotates through every pending receipt", async () => {
    const jobs = Array.from({ length: 17 }, (_, index) =>
      job("queued", `job_${String(index).padStart(2, "0")}`, `acct_${index}`),
    );
    bridge.getCheckinJob.mockImplementation(async (id: string) =>
      jobs.find((value) => value.id === id),
    );
    await render();
    await act(async () => actions.adopt(jobs));
    await tick();
    expect(bridge.getCheckinJob).toHaveBeenCalledTimes(MAX_POLL_REQUESTS);
    await tick();
    expect(bridge.getCheckinJob).toHaveBeenCalledTimes(MAX_POLL_REQUESTS * 2);
    await tick();
    expect(
      new Set(bridge.getCheckinJob.mock.calls.map(([id]) => id)).size,
    ).toBe(17);
  });

  it("does not overlap a retired poll with a new target set", async () => {
    const pending = deferred<CheckinJob>();
    bridge.getCheckinJob.mockReturnValue(pending.promise);
    await render();
    await act(async () => actions.adopt([job()]));
    await tick();
    await act(async () =>
      actions.adopt([job("queued", "job_two", "acct_two")]),
    );
    await tick(4);
    expect(bridge.getCheckinJob).toHaveBeenCalledOnce();
    await act(async () => pending.resolve(job()));
    bridge.getCheckinJob.mockImplementation(async (id: string) =>
      job("queued", id),
    );
    await tick();
    expect(bridge.getCheckinJob).toHaveBeenCalledTimes(3);
  });

  it("blocks replacement writes until the original lost receipt is retried", async () => {
    bridge.createCheckinJob.mockRejectedValueOnce(
      new CheckinBridgeError("transport", { resume }),
    );
    await render();
    await act(async () => {
      actions.run("check_in", [account.id]);
    });
    expect(actions.accountBusy(account.id)).toBe(true);
    expect(actions.canRun("status_refresh", account.id)).toBe(false);
    await render({ accounts: [{ ...account, revision: 3 }] });
    await act(async () => {
      expect(actions.run("check_in", [account.id])).toBe(false);
    });
    await act(async () => actions.retry(account.id));
    expect(bridge.createCheckinJob).toHaveBeenCalledTimes(2);
    expect(bridge.createCheckinJob).toHaveBeenLastCalledWith(
      { action: "check_in", accounts: [account.id], expected_revision: 2 },
      resume,
    );
  });

  it("never sends another Core a retry id issued by the previous Core", async () => {
    bridge.createCheckinJob.mockRejectedValueOnce(
      new CheckinBridgeError("transport", { resume }),
    );
    await render();
    await act(async () => {
      actions.run("check_in", [account.id]);
    });
    await render({ sessionKey: "core-two" });
    await act(async () => {
      actions.run("check_in", [account.id]);
    });
    expect(bridge.createCheckinJob).toHaveBeenLastCalledWith(
      { action: "check_in", accounts: [account.id], expected_revision: 2 },
      null,
    );
  });

  it.each(["uncertain", "cancelled"] as const)(
    "does not resubmit a dispatched %s after an unsuccessful status read",
    async (status) => {
      await render();
      await act(async () =>
        actions.adopt([
          { ...job(status), dispatched: true },
          {
            ...job("retryable_failure", "job_refresh"),
            action: "status_refresh",
            created_at: new Date(2000).toISOString(),
          },
        ]),
      );
      expect(actions.canRun("check_in", account.id)).toBe(false);
      expect(actions.canRun("status_refresh", account.id)).toBe(true);
    },
  );

  it("only allows a new day's submission after a later authoritative status read", async () => {
    const firstDay = new Date(0).toISOString().slice(0, 10);
    const nextDay = new Date(86_400_000).toISOString().slice(0, 10);
    const read: CheckinJob = {
      ...job("not_checked", "job_refresh"),
      action: "status_refresh",
      proof_source: "status_read",
      site_date: firstDay,
      created_at: new Date(2000).toISOString(),
    };
    await render();
    await act(async () =>
      actions.adopt([
        { ...job("uncertain"), dispatched: true, site_date: firstDay },
        read,
      ]),
    );
    expect(actions.canRun("check_in", account.id)).toBe(false);
    await act(async () =>
      actions.adopt([
        {
          ...read,
          id: "job_nextday",
          site_date: nextDay,
          created_at: new Date(86_400_000).toISOString(),
        },
      ]),
    );
    expect(actions.canRun("check_in", account.id)).toBe(true);
  });

  it("reconciles batch children monotonically after an individual cancellation and stale parent read", async () => {
    const batch: CheckinJob = {
      ...job("running", "batch_one"),
      account_id: undefined,
      children: [job(), job("queued", "job_two", "acct_two")],
    };
    bridge.cancelCheckinJob.mockResolvedValue(job("cancelled"));
    await render();
    await act(async () => actions.adopt([batch]));
    await act(async () => actions.cancel(job()));
    await act(async () => actions.adopt([batch]));
    expect(actions.latestBatch?.children[0].status).toBe("cancelled");
    expect(actions.records[0].children[0].status).toBe("cancelled");
    expect(actions.receipt("job_one")?.status).toBe("cancelled");
  });

  it("pins cancellation retries and clears them when their account is deleted", async () => {
    const cancelResume = { ...resume, op: "cancel_job" as const };
    bridge.cancelCheckinJob.mockRejectedValue(
      new CheckinBridgeError("transport", { resume: cancelResume }),
    );
    await render();
    await act(async () => actions.adopt([job()]));
    await act(async () => actions.cancel(job()));
    await act(async () => actions.retry(account.id));
    expect(bridge.cancelCheckinJob).toHaveBeenLastCalledWith(
      "job_one",
      cancelResume,
    );
    await act(async () => actions.forgetAccount(account.id));
    await act(async () => actions.retry(account.id));
    expect(bridge.cancelCheckinJob).toHaveBeenCalledTimes(2);
  });

  it("never restores deleted accounts from late polling responses", async () => {
    const pending = deferred<CheckinJob>();
    bridge.getCheckinJob.mockReturnValue(pending.promise);
    await render();
    await act(async () => actions.adopt([job()]));
    await tick();
    await act(async () => actions.forgetAccount(account.id));
    await act(async () => pending.resolve(job("success")));
    expect(actions.records).toHaveLength(0);
    expect(onSettled).not.toHaveBeenCalled();
  });
});
