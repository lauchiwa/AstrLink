import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import {
  CheckinBridgeError,
  cancelCheckinJob,
  createCheckinJob,
  describeCheckinError,
  getCheckinJob,
  type CheckinResume,
} from "./bridge";
import { checkinT } from "./i18n";
import {
  isTerminal,
  type CheckinAccount,
  type CheckinJob,
  type JobAction,
} from "./model";

export const MAX_BATCH_ACCOUNTS = 25;
export const POLL_INTERVAL_MS = 3_000;
export const MAX_POLL_REQUESTS = 8;
export const BATCH_SLOT = "@batch";
export const readSlot = (id: string) => `read:${id}`;
export const canCheckIn = (account: CheckinAccount) =>
  account.state === "connected";
export const canRefresh = (account: CheckinAccount) =>
  canCheckIn(account) || account.state === "manual_required";
export const isCancellable = (job: CheckinJob): boolean =>
  !isTerminal(job.status) &&
  (job.account_id === undefined
    ? job.children.some(
        (child) => !child.dispatched && !isTerminal(child.status),
      )
    : !job.dispatched);

export interface ActionMessage {
  tone: "error" | "notice";
  text: string;
  retry?: boolean;
}
interface Write {
  logical: string;
  slot: string;
  accounts: string[];
  invalidate?: string[];
  send: (resume: CheckinResume | null) => Promise<CheckinJob>;
  notice?: (answer: CheckinJob) => string | null;
}
interface State {
  key: string | null;
  receipts: Record<string, CheckinJob>;
  messages: Record<string, ActionMessage>;
  inFlight: Record<string, Write>;
  pending: Record<string, Write>;
}
const empty = (key: string | null): State => ({
  key,
  receipts: {},
  messages: {},
  inFlight: {},
  pending: {},
});
function omit<T>(
  value: Record<string, T>,
  keys: readonly string[],
): Record<string, T> {
  const next = { ...value };
  for (const key of keys) delete next[key];
  return next;
}

/** Old list/poll responses cannot undo a durable terminal/dispatch transition. */
function advance(
  previous: CheckinJob | undefined,
  next: CheckinJob,
): CheckinJob {
  if (!previous) return next;
  if (isTerminal(previous.status)) return previous;
  if (previous.dispatched && !next.dispatched) return previous;
  if (previous.status === "running" && next.status === "queued")
    return previous;
  return next;
}
function merge(state: State, job: CheckinJob): State {
  const receipts = { ...state.receipts };
  for (const item of [...job.children, job])
    receipts[item.id] = advance(receipts[item.id], item);
  for (const parent of Object.values(receipts)) {
    if (parent.children.length)
      receipts[parent.id] = {
        ...parent,
        children: parent.children.map((child) => receipts[child.id] ?? child),
      };
  }
  return { ...state, receipts };
}
function latest(state: State, account: string): CheckinJob | undefined {
  return Object.values(state.receipts)
    .filter((job) => job.account_id === account)
    .reduce<CheckinJob | undefined>(
      (before, job) =>
        !before || Date.parse(job.created_at) >= Date.parse(before.created_at)
          ? job
          : before,
      undefined,
    );
}
function busy(state: State, account: string): boolean {
  return (
    Object.values(state.receipts).some(
      (job) => job.account_id === account && !isTerminal(job.status),
    ) ||
    [...Object.values(state.inFlight), ...Object.values(state.pending)].some(
      (write) => write.accounts.includes(account),
    )
  );
}
function unresolvedSubmission(state: State, accountID: string): boolean {
  const jobs = Object.values(state.receipts).filter(
    (job) => job.account_id === accountID,
  );
  return jobs.some(
    (job) =>
      job.action === "check_in" &&
      job.dispatched &&
      (job.status === "uncertain" || job.status === "cancelled") &&
      !jobs.some(
        (read) =>
          read.action === "status_refresh" &&
          read.proof_source === "status_read" &&
          ["success", "already_checked", "not_checked"].includes(read.status) &&
          Date.parse(read.created_at) > Date.parse(job.created_at) &&
          !!read.site_date &&
          !!job.site_date &&
          read.site_date > job.site_date,
      ),
  );
}
function canRunIn(
  state: State,
  accounts: readonly CheckinAccount[],
  active: boolean,
  action: JobAction,
  accountID: string,
): boolean {
  const account = accounts.find((value) => value.id === accountID);
  return (
    active &&
    !!account &&
    Object.keys(state.inFlight).length < MAX_POLL_REQUESTS &&
    !busy(state, accountID) &&
    (action === "status_refresh"
      ? canRefresh(account)
      : canCheckIn(account) && !unresolvedSubmission(state, accountID))
  );
}
function pollTargets(state: State): string[] {
  const parents = Object.values(state.receipts).filter(
    (job) => job.account_id === undefined && !isTerminal(job.status),
  );
  const covered = new Set(
    parents.flatMap((job) => job.children.map((child) => child.id)),
  );
  return Object.values(state.receipts)
    .filter((job) => !isTerminal(job.status) && !covered.has(job.id))
    .map((job) => job.id)
    .sort();
}
function topLevel(state: State): CheckinJob[] {
  const children = new Set(
    Object.values(state.receipts).flatMap((job) =>
      job.children.map((child) => child.id),
    ),
  );
  return Object.values(state.receipts)
    .filter((job) => !children.has(job.id))
    .sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at));
}

export interface CheckinActions {
  latest(accountID: string): CheckinJob | undefined;
  latestBatch: CheckinJob | undefined;
  records: CheckinJob[];
  receipt(id: string): CheckinJob | undefined;
  forgotten(id: string): boolean;
  message(slot: string): ActionMessage | undefined;
  accountBusy(accountID: string): boolean;
  batchBusy: boolean;
  writeBusy: boolean;
  cancelling(jobID: string): boolean;
  canRun(action: JobAction, accountID: string): boolean;
  run(action: JobAction, accountIDs: readonly string[]): boolean;
  cancel(job: CheckinJob): void;
  retry(slot: string): void;
  adopt(jobs: readonly CheckinJob[], replace?: boolean): void;
  forgetAccount(accountID: string): void;
}

/** Writes are explicit; polling only reads persisted local Core receipts. */
export function useCheckinActions(props: {
  active: boolean;
  accounts: readonly CheckinAccount[];
  sessionKey: string | null;
  onSettled: (accountIDs: string[]) => void;
}): CheckinActions {
  const [stored, setStored] = useState(() => empty(props.sessionKey));
  const state = useRef(stored);
  const input = useRef(props);
  const mounted = useRef(false);
  const generation = useRef(0);
  const versions = useRef(new Map<string, number>());
  const forgotten = useRef(new Set<string>());
  const removedAccounts = useRef(new Set<string>());
  const resumes = useRef(new Map<string, CheckinResume>());
  const retries = useRef(new Map<string, Write>());
  const offset = useRef(0);
  const pollInFlight = useRef(false);
  useLayoutEffect(() => {
    input.current = props;
  });
  useLayoutEffect(() => {
    mounted.current = true;
    generation.current += 1;
    if (state.current.key !== props.sessionKey) {
      state.current = empty(props.sessionKey);
      forgotten.current.clear();
      removedAccounts.current.clear();
      versions.current.clear();
      retries.current.clear();
      resumes.current.clear();
      offset.current = 0;
    } else {
      state.current = { ...state.current, inFlight: {} };
    }
    setStored(state.current);
    return () => {
      mounted.current = false;
      generation.current += 1;
    };
  }, [props.active, props.sessionKey]);

  const live = useCallback(
    (epoch: number) =>
      mounted.current && input.current.active && generation.current === epoch,
    [],
  );
  const publish = useCallback((next: State, notify = false) => {
    if (!mounted.current) return;
    const before = state.current;
    state.current = next;
    setStored(next);
    if (notify) {
      const settled = Object.values(next.receipts)
        .filter(
          (job) =>
            job.account_id !== undefined &&
            isTerminal(job.status) &&
            !isTerminal(before.receipts[job.id]?.status ?? "queued"),
        )
        .map((job) => job.account_id!);
      if (settled.length) input.current.onSettled([...new Set(settled)]);
    }
  }, []);
  const allowedReceipt = useCallback(
    (job: CheckinJob) =>
      !forgotten.current.has(job.id) &&
      ![job, ...job.children].some(
        (item) =>
          item.account_id && removedAccounts.current.has(item.account_id),
      ),
    [],
  );
  const adopt = useCallback(
    (jobs: readonly CheckinJob[], replace = false) => {
      if (!mounted.current) return;
      const retained = new Set(
        jobs.flatMap((job) => [
          job.id,
          ...job.children.map((child) => child.id),
        ]),
      );
      const base = replace
        ? {
            ...state.current,
            receipts: Object.fromEntries(
              Object.entries(state.current.receipts).filter(
                ([id, job]) => retained.has(id) || !isTerminal(job.status),
              ),
            ),
          }
        : state.current;
      publish(
        jobs.reduce(
          (next, job) => (allowedReceipt(job) ? merge(next, job) : next),
          base,
        ),
      );
    },
    [allowedReceipt, publish],
  );
  const removeReceipt = useCallback((next: State, id: string) => {
    const ids = [
      id,
      ...(next.receipts[id]?.children.map((child) => child.id) ?? []),
    ];
    for (const value of ids) forgotten.current.add(value);
    return {
      ...next,
      receipts: omit(next.receipts, ids),
      messages: omit(next.messages, ids.map(readSlot)),
    };
  }, []);
  const forgetAccount = useCallback(
    (account: string) => {
      removedAccounts.current.add(account);
      let next = state.current;
      for (const job of Object.values(next.receipts)) {
        if (
          job.account_id === account ||
          job.children.some((child) => child.account_id === account)
        )
          next = removeReceipt(next, job.id);
      }
      for (const [slot, write] of retries.current) {
        if (write.accounts.includes(account)) {
          retries.current.delete(slot);
          resumes.current.delete(write.logical);
          next = {
            ...next,
            pending: omit(next.pending, [slot]),
            messages: omit(next.messages, [slot]),
          };
        }
      }
      publish({ ...next, messages: omit(next.messages, [account]) });
    },
    [publish, removeReceipt],
  );

  const canRun = useCallback((action: JobAction, accountID: string) => {
    return (
      mounted.current &&
      canRunIn(
        state.current,
        input.current.accounts,
        input.current.active,
        action,
        accountID,
      )
    );
  }, []);
  const perform = useCallback(
    (write: Write): boolean => {
      const epoch = generation.current;
      if (
        !live(epoch) ||
        Object.values(state.current.inFlight).some(
          (other) =>
            other.logical === write.logical || other.slot === write.slot,
        ) ||
        (state.current.pending[write.slot] &&
          state.current.pending[write.slot].logical !== write.logical) ||
        Object.keys(state.current.inFlight).length >= MAX_POLL_REQUESTS
      )
        return false;
      for (const id of write.invalidate ?? [])
        versions.current.set(id, (versions.current.get(id) ?? 0) + 1);
      retries.current.delete(write.slot);
      publish({
        ...state.current,
        pending: omit(state.current.pending, [write.slot]),
        messages: omit(state.current.messages, [write.slot]),
        inFlight: { ...state.current.inFlight, [write.logical]: write },
      });
      void (async () => {
        try {
          const job = await write.send(
            resumes.current.get(write.logical) ?? null,
          );
          if (!live(epoch)) return;
          resumes.current.delete(write.logical);
          let next = {
            ...state.current,
            inFlight: omit(state.current.inFlight, [write.logical]),
          };
          if (allowedReceipt(job)) next = merge(next, job);
          const notice = write.notice?.(job);
          if (notice)
            next.messages = {
              ...next.messages,
              [write.slot]: { tone: "notice", text: notice },
            };
          publish(next, true);
        } catch (error) {
          if (!live(epoch)) return;
          const resume =
            error instanceof CheckinBridgeError ? error.resume : null;
          if (resume) {
            resumes.current.set(write.logical, resume);
            retries.current.set(write.slot, write);
          } else resumes.current.delete(write.logical);
          publish({
            ...state.current,
            pending: resume
              ? { ...state.current.pending, [write.slot]: write }
              : state.current.pending,
            inFlight: omit(state.current.inFlight, [write.logical]),
            messages: {
              ...state.current.messages,
              [write.slot]: {
                tone: "error",
                text: describeCheckinError(error),
                retry: !!resume,
              },
            },
          });
        }
      })();
      return true;
    },
    [allowedReceipt, live, publish],
  );
  const run = useCallback(
    (action: JobAction, ids: readonly string[]) => {
      const accounts = [...new Set(ids)].sort();
      if (
        !accounts.length ||
        accounts.length > MAX_BATCH_ACCOUNTS ||
        accounts.some((id) => !canRun(action, id))
      )
        return false;
      const body = {
        action,
        accounts,
        ...(accounts.length === 1
          ? {
              expected_revision: input.current.accounts.find(
                (account) => account.id === accounts[0],
              )!.revision,
            }
          : {}),
      };
      return perform({
        logical: JSON.stringify(body),
        accounts,
        slot: accounts.length === 1 ? accounts[0] : BATCH_SLOT,
        send: (resume) => createCheckinJob(body, resume),
      });
    },
    [canRun, perform],
  );
  const cancel = useCallback(
    (value: CheckinJob) => {
      const job = state.current.receipts[value.id] ?? value;
      if (!isCancellable(job)) return;
      const batch = job.account_id === undefined;
      perform({
        logical: `cancel:${job.id}`,
        accounts: [job, ...job.children].flatMap((item) =>
          item.account_id ? [item.account_id] : [],
        ),
        slot: job.account_id ?? BATCH_SLOT,
        invalidate: [job.id, ...job.children.map((child) => child.id)],
        send: async (resume) => {
          const answer = await cancelCheckinJob(job.id, resume);
          if (answer.id !== job.id)
            throw new CheckinBridgeError("malformed", { resume });
          return answer;
        },
        notice: (answer) =>
          answer.dispatched
            ? checkinT(
                batch
                  ? "actions.cancelBatchDispatched"
                  : "actions.cancelDispatched",
              )
            : null,
      });
    },
    [perform],
  );
  const retry = useCallback(
    (slot: string) => {
      const write = retries.current.get(slot);
      if (
        write &&
        write.accounts.every((id) => !removedAccounts.current.has(id))
      )
        perform(write);
    },
    [perform],
  );

  const view =
    stored.key === props.sessionKey ? stored : empty(props.sessionKey);
  const targetKey = props.active ? pollTargets(view).join(",") : "";
  useEffect(() => {
    if (!targetKey) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const epoch = generation.current;
    const tick = async () => {
      if (stopped || !live(epoch)) return;
      if (pollInFlight.current) {
        timer = setTimeout(() => void tick(), POLL_INTERVAL_MS);
        return;
      }
      const targets = pollTargets(state.current).filter(
        (id) => !forgotten.current.has(id),
      );
      if (!targets.length) return;
      const selected = Array.from(
        { length: Math.min(targets.length, MAX_POLL_REQUESTS) },
        (_, index) => targets[(offset.current + index) % targets.length],
      );
      offset.current = (offset.current + selected.length) % targets.length;
      const stamps = selected.map((id) => versions.current.get(id) ?? 0);
      pollInFlight.current = true;
      const answers = await Promise.allSettled(
        selected.map((id) => getCheckinJob(id)),
      );
      pollInFlight.current = false;
      if (stopped || !live(epoch)) return;
      let next = state.current;
      answers.forEach((answer, index) => {
        const id = selected[index];
        if (
          stamps[index] !== (versions.current.get(id) ?? 0) ||
          forgotten.current.has(id)
        )
          return;
        if (
          answer.status === "fulfilled" &&
          answer.value.id === id &&
          allowedReceipt(answer.value)
        ) {
          next = merge(next, answer.value);
          next.messages = omit(next.messages, [readSlot(id)]);
        } else if (
          answer.status === "rejected" &&
          answer.reason instanceof CheckinBridgeError &&
          answer.reason.code === "not_found"
        ) {
          next = removeReceipt(next, id);
        } else {
          next = {
            ...next,
            messages: {
              ...next.messages,
              [readSlot(id)]: {
                tone: "error",
                text: checkinT("actions.pollFailed"),
              },
            },
          };
        }
      });
      publish(next, true);
      if (!stopped && live(epoch) && pollTargets(state.current).length)
        timer = setTimeout(() => void tick(), POLL_INTERVAL_MS);
    };
    timer = setTimeout(() => void tick(), POLL_INTERVAL_MS);
    return () => {
      stopped = true;
      if (timer !== undefined) clearTimeout(timer);
    };
  }, [
    allowedReceipt,
    live,
    props.sessionKey,
    publish,
    removeReceipt,
    targetKey,
  ]);

  const records = topLevel(view);
  return {
    records,
    receipt: (id) => view.receipts[id],
    forgotten: (id) => forgotten.current.has(id),
    latest: (id) => latest(view, id),
    latestBatch: records.find((job) => job.account_id === undefined),
    message: (slot) => view.messages[slot],
    accountBusy: (id) => busy(view, id),
    batchBusy: [
      ...Object.values(view.inFlight),
      ...Object.values(view.pending),
    ].some((write) => write.slot === BATCH_SLOT),
    writeBusy: Object.keys(view.inFlight).length > 0,
    cancelling: (id) => !!view.inFlight[`cancel:${id}`],
    canRun: (action, id) =>
      canRunIn(view, props.accounts, props.active, action, id),
    run,
    cancel,
    retry,
    adopt,
    forgetAccount,
  };
}
