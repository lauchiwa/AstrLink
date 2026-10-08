import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import { Plus, RefreshCw } from "@/components/icons";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { EmptyState } from "@/components/EmptyState";
import { FormMessage } from "@/components/FormMessage";
import { LoadingState } from "@/components/LoadingState";
import { Button } from "@/components/ui/button";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { PageHeader } from "@/PageHeader";

import {
  CheckinBridgeError,
  describeCheckinError,
  getCheckinAccount,
  listCheckinAccounts,
  listCheckinJobs,
  loadCheckinAvailability,
  updateCheckinSettings,
  type CheckinAvailability,
  type CheckinResume,
} from "./bridge";
import { AccountDialog, type CheckinServiceCatalog } from "./AccountDialog";
import { ConnectAccountDialog } from "./ConnectAccountDialog";
import { DeleteAccountDialog } from "./DeleteAccountDialog";
import { AccountList, JobList, type ListState } from "./CheckinLists";
import {
  AccountActions,
  AccountFeedback,
  AccountSelect,
  BatchSummary,
  BatchToolbar,
  RecordActions,
  batchEligible,
  hasFeedback,
} from "./actions";
import {
  MAX_BATCH_ACCOUNTS,
  MAX_POLL_REQUESTS,
  useCheckinActions,
} from "./use-checkin-actions";
import { checkinHasKey, useCheckinT } from "./i18n";
import type { CheckinAccount, CheckinJob, CheckinStatus } from "./model";

type Probe =
  | { kind: "loading" }
  | { kind: "error"; message: string }
  | CheckinAvailability;

type Tab = "accounts" | "records";
type AccountEditor =
  | { kind: "edit"; account: CheckinAccount | null }
  | { kind: "connect"; account: CheckinAccount }
  | { kind: "delete"; account: CheckinAccount };

function emptyList<T>(): ListState<T> {
  return { status: "idle", items: [], error: null, loadingMore: false };
}

function settled<T>(
  result: PromiseSettledResult<{ items: T[]; next_cursor?: string }>,
): ListState<T> {
  return result.status === "fulfilled"
    ? {
        status: "ready",
        items: result.value.items,
        cursor: result.value.next_cursor,
        error: null,
        loadingMore: false,
      }
    : {
        status: "error",
        items: [],
        error: describeCheckinError(result.reason),
        loadingMore: false,
      };
}

/**
 * Check-in workspace. Nothing here runs until the page is opened:
 * it probes the extension once, reads accounts and records only when the
 * extension is ready, and drops every late answer once the page is left.
 */
export function CheckinWorkspace({
  coreSessionKey,
  isReady,
  services,
  servicesReady,
}: CheckinServiceCatalog & {
  coreSessionKey: string | null;
  isReady: boolean;
}) {
  const t = useCheckinT();
  const [probe, setProbe] = useState<Probe>({ kind: "loading" });
  const [accounts, setAccounts] = useState(emptyList<CheckinAccount>);
  const [jobs, setJobs] = useState(emptyList<CheckinJob>);
  const [tab, setTab] = useState<Tab>("accounts");
  const [switching, setSwitching] = useState(false);
  const [switchError, setSwitchError] = useState<string | null>(null);
  const [confirmDisable, setConfirmDisable] = useState(false);
  const [editor, setEditor] = useState<AccountEditor | null>(null);
  const [selection, setSelection] = useState<string[]>([]);
  // The same logical switch keeps its request_id across retries.
  const switchResume = useRef<CheckinResume | null>(null);
  // Layout cleanup invalidates the old Core/page before passive effects or
  // promise continuations can publish a stale result.
  const active = useRef(false);
  const generation = useRef(0);
  const switchInFlight = useRef(false);
  const accountPageInFlight = useRef<number | null>(null);
  const jobPageInFlight = useRef<number | null>(null);
  const accountRefreshQueue = useRef({ epoch: -1, ids: new Set<string>() });
  const accountRefreshInFlight = useRef(false);
  const status: CheckinStatus | null =
    probe.kind === "present" ? probe.status : null;
  const listsReady = !!(isReady && status?.enabled && status.storage_ready);
  const busy =
    probe.kind === "loading" ||
    accounts.status === "loading" ||
    switching ||
    editor !== null;
  const refreshSettledAccounts = useCallback((ids: string[]) => {
    if (!active.current) return;
    if (accountRefreshQueue.current.epoch !== generation.current) {
      accountRefreshQueue.current = {
        epoch: generation.current,
        ids: new Set(),
      };
    }
    for (const id of ids) accountRefreshQueue.current.ids.add(id);
    if (accountRefreshInFlight.current) return;
    accountRefreshInFlight.current = true;
    void (async () => {
      try {
        while (active.current) {
          const queue = accountRefreshQueue.current;
          if (queue.epoch !== generation.current || !queue.ids.size) break;
          const selected = [...queue.ids].slice(0, MAX_POLL_REQUESTS);
          for (const id of selected) queue.ids.delete(id);
          const results = await Promise.allSettled(
            selected.map((id) => getCheckinAccount(id)),
          );
          if (!active.current || queue.epoch !== generation.current) continue;
          const updates = new Map(
            results.flatMap((result, index) =>
              result.status === "fulfilled" &&
              result.value.id === selected[index]
                ? [[result.value.id, result.value] as const]
                : [],
            ),
          );
          setAccounts((state) => ({
            ...state,
            items: state.items.map((account) => {
              const next = updates.get(account.id);
              return next && next.revision >= account.revision ? next : account;
            }),
          }));
        }
      } finally {
        accountRefreshInFlight.current = false;
      }
    })();
  }, []);
  const actions = useCheckinActions({
    active: listsReady && !switching,
    accounts: accounts.items,
    sessionKey: coreSessionKey,
    onSettled: refreshSettledAccounts,
  });
  const eligible = batchEligible(accounts.items, actions);
  const eligibleKey = eligible.join(",");
  const selected = selection.filter((id) => eligible.includes(id));
  useLayoutEffect(() => {
    const allowed = new Set(eligibleKey.split(","));
    setSelection((previous) =>
      previous.every((id) => allowed.has(id))
        ? previous
        : previous.filter((id) => allowed.has(id)),
    );
  }, [eligibleKey]);

  useLayoutEffect(() => {
    active.current = isReady;
    generation.current += 1;
    switchInFlight.current = false;
    setSwitching(false);
    setSwitchError(null);
    setConfirmDisable(false);
    setEditor(null);
    setSelection([]);
    setProbe({ kind: "loading" });
    setAccounts(emptyList());
    setJobs(emptyList());
    return () => {
      active.current = false;
      generation.current += 1;
    };
  }, [coreSessionKey, isReady]);

  const refresh = useCallback(async () => {
    if (!active.current || switchInFlight.current) return;
    const current = ++generation.current;
    const live = () => active.current && generation.current === current;
    setProbe({ kind: "loading" });
    let next: CheckinAvailability;
    try {
      next = await loadCheckinAvailability();
    } catch (error) {
      if (live()) {
        setProbe({ kind: "error", message: describeCheckinError(error) });
      }
      return;
    }
    if (!live()) return;
    setProbe(next);
    if (
      next.kind !== "present" ||
      !next.status.enabled ||
      !next.status.storage_ready
    ) {
      setAccounts(emptyList());
      setJobs(emptyList());
      return;
    }
    setAccounts((state) => ({ ...state, status: "loading", error: null }));
    setJobs((state) => ({ ...state, status: "loading", error: null }));
    const [accountPage, jobPage] = await Promise.allSettled([
      listCheckinAccounts(),
      listCheckinJobs(),
    ]);
    if (!live()) return;
    setAccounts((previous) => {
      const next = settled(accountPage);
      const known = new Map(
        previous.items.map((account) => [account.id, account]),
      );
      return {
        ...next,
        items: next.items.map((account) => {
          const newer = known.get(account.id);
          return newer && newer.revision > account.revision ? newer : account;
        }),
      };
    });
    if (jobPage.status === "fulfilled")
      actions.adopt(jobPage.value.items, true);
    setJobs(settled(jobPage));
  }, [actions.adopt]);

  useEffect(() => {
    if (isReady) void refresh();
  }, [coreSessionKey, isReady, refresh]);

  const loadMoreAccounts = async () => {
    const current = generation.current;
    const cursor = accounts.cursor;
    if (
      !active.current ||
      switchInFlight.current ||
      !cursor ||
      accounts.status !== "ready" ||
      accountPageInFlight.current === current
    )
      return;
    accountPageInFlight.current = current;
    setAccounts((state) => ({ ...state, loadingMore: true, error: null }));
    try {
      const page = await listCheckinAccounts({ cursor });
      if (generation.current !== current) return;
      setAccounts((state) => ({
        ...state,
        items: [
          ...new Map(
            [...state.items, ...page.items].map((item) => [item.id, item]),
          ).values(),
        ],
        cursor: page.next_cursor,
        loadingMore: false,
      }));
    } catch (error) {
      if (generation.current !== current) return;
      setAccounts((state) => ({
        ...state,
        error: describeCheckinError(error),
        loadingMore: false,
      }));
    } finally {
      if (accountPageInFlight.current === current)
        accountPageInFlight.current = null;
    }
  };

  const loadMoreJobs = async () => {
    const current = generation.current;
    const cursor = jobs.cursor;
    if (
      !active.current ||
      switchInFlight.current ||
      !cursor ||
      jobs.status !== "ready" ||
      jobPageInFlight.current === current
    )
      return;
    jobPageInFlight.current = current;
    setJobs((state) => ({ ...state, loadingMore: true, error: null }));
    try {
      const page = await listCheckinJobs({ cursor });
      if (generation.current !== current) return;
      actions.adopt(page.items);
      setJobs((state) => ({
        ...state,
        items: [...state.items, ...page.items],
        cursor: page.next_cursor,
        loadingMore: false,
      }));
    } catch (error) {
      if (generation.current !== current) return;
      setJobs((state) => ({
        ...state,
        error: describeCheckinError(error),
        loadingMore: false,
      }));
    } finally {
      if (jobPageInFlight.current === current) jobPageInFlight.current = null;
    }
  };

  const setEnabled = async (enabled: boolean) => {
    if (!active.current || switchInFlight.current) return;
    switchInFlight.current = true;
    // Leaving this page/Core generation must never start another refresh.
    const current = generation.current;
    const live = () => active.current && generation.current === current;
    setSwitching(true);
    setSwitchError(null);
    try {
      await updateCheckinSettings(enabled, switchResume.current);
    } catch (error) {
      if (!live()) return;
      switchResume.current =
        error instanceof CheckinBridgeError ? error.resume : null;
      setSwitchError(describeCheckinError(error));
      switchInFlight.current = false;
      setSwitching(false);
      return;
    }
    if (!live()) return;
    switchResume.current = null;
    switchInFlight.current = false;
    setSwitching(false);
    await refresh();
  };

  const accountSaved = (account: CheckinAccount) => {
    generation.current += 1;
    setEditor(null);
    setAccounts((state) => ({
      ...state,
      status: "ready",
      error: null,
      loadingMore: false,
      items: state.items.some((item) => item.id === account.id)
        ? state.items.map((item) => (item.id === account.id ? account : item))
        : [account, ...state.items],
    }));
    setJobs((state) => ({ ...state, loadingMore: false }));
  };
  const accountDeleted = (id: string) => {
    generation.current += 1;
    actions.forgetAccount(id);
    setSelection((previous) => previous.filter((value) => value !== id));
    setEditor(null);
    setAccounts((state) => ({
      ...state,
      loadingMore: false,
      items: state.items.filter((item) => item.id !== id),
    }));
    setJobs((state) => ({
      ...state,
      loadingMore: false,
      items: state.items.filter(
        (item) =>
          item.account_id !== id &&
          !item.children.some((child) => child.account_id === id),
      ),
    }));
  };

  return (
    <section
      aria-labelledby="checkin-workspace-heading"
      className="@container gutter-frame flex min-h-0 w-full min-w-0 flex-1 flex-col overflow-hidden"
    >
      <PageHeader
        actions={
          <>
            {listsReady ? (
              <Button
                disabled={busy || actions.writeBusy}
                onClick={() => setConfirmDisable(true)}
                size="sm"
                type="button"
                variant="ghost"
              >
                {switching ? t("workspace.disabling") : t("workspace.disable")}
              </Button>
            ) : null}
            <Button
              disabled={!isReady || busy || actions.writeBusy}
              onClick={() => void refresh()}
              size="sm"
              type="button"
              variant="outline"
            >
              <RefreshCw
                className={
                  probe.kind === "loading"
                    ? "animate-spin motion-reduce:animate-none"
                    : undefined
                }
              />
              {probe.kind === "loading"
                ? t("workspace.refreshing")
                : t("workspace.refresh")}
            </Button>
          </>
        }
        title={t("workspace.title")}
        titleId="checkin-workspace-heading"
      />

      {switchError ? (
        <FormMessage className="mb-3" tone="error">
          {switchError}
        </FormMessage>
      ) : null}

      {!isReady ? (
        <EmptyState
          description={t("workspace.waitingHint")}
          title={t("workspace.waiting")}
        />
      ) : probe.kind === "loading" ? (
        <div className="flex justify-center py-10">
          <LoadingState label={t("workspace.loading")} />
        </div>
      ) : probe.kind === "error" ? (
        <EmptyState
          action={
            <Button
              onClick={() => void refresh()}
              size="sm"
              type="button"
              variant="outline"
            >
              {t("workspace.retry")}
            </Button>
          }
          description={probe.message}
          title={t("workspace.readFailed")}
        />
      ) : probe.kind === "desktop_only" ? (
        <EmptyState
          description={t("bridge.desktopOnly")}
          title={t("workspace.unavailable")}
        />
      ) : probe.kind === "unavailable" ? (
        <EmptyState
          description={t("workspace.unavailableHint")}
          title={t("workspace.unavailable")}
        />
      ) : probe.kind === "unsupported" ? (
        <EmptyState
          description={t("workspace.unsupportedHint")}
          title={t("workspace.unsupported")}
        />
      ) : !probe.status.enabled || !probe.status.storage_ready ? (
        <EmptyState
          action={
            <Button
              disabled={switching}
              onClick={() => void setEnabled(true)}
              type="button"
            >
              {switching ? t("enable.enabling") : t("enable.action")}
            </Button>
          }
          description={
            <>
              {probe.status.enabled
                ? t("workspace.startingHint")
                : t("enable.description")}
              {probe.status.last_error_code ? (
                <span className="mt-2 block text-danger-foreground">
                  {initErrorText(t, probe.status.last_error_code)}
                </span>
              ) : null}
            </>
          }
          title={
            probe.status.enabled ? t("workspace.starting") : t("enable.title")
          }
        />
      ) : (
        <Tabs
          className="flex min-h-0 min-w-0 flex-1 flex-col gap-3"
          onValueChange={(value) => setTab(value as Tab)}
          value={tab}
        >
          <div className="flex shrink-0 flex-wrap items-center justify-between gap-2">
            <TabsList aria-label={t("workspace.tabsLabel")}>
              <TabsTrigger value="accounts">
                {t("workspace.accountsTab")}
              </TabsTrigger>
              <TabsTrigger value="records">
                {t("workspace.recordsTab")}
              </TabsTrigger>
            </TabsList>
            {tab === "accounts" ? (
              <div className="flex min-w-0 flex-wrap items-center gap-2">
                <BatchToolbar
                  actions={actions}
                  eligible={eligible}
                  selection={selected}
                  disabled={busy}
                  onSelectionChange={setSelection}
                />
                <Button
                  size="sm"
                  disabled={busy || accounts.status !== "ready"}
                  onClick={() => setEditor({ kind: "edit", account: null })}
                >
                  <Plus />
                  {t("dialog.createTitle")}
                </Button>
              </div>
            ) : null}
          </div>
          <TabsContent
            className="gutter-scroller min-h-0 min-w-0 flex-1 overflow-y-auto"
            data-tab-scroller
            forceMount
            hidden={tab !== "accounts"}
            value="accounts"
          >
            <BatchSummary actions={actions} />
            <AccountList
              decorate={(account) => ({
                leading: (
                  <AccountSelect
                    account={account}
                    checked={selected.includes(account.id)}
                    disabled={
                      busy ||
                      !eligible.includes(account.id) ||
                      (!selected.includes(account.id) &&
                        selected.length >= MAX_BATCH_ACCOUNTS)
                    }
                    onCheckedChange={(checked) =>
                      setSelection((previous) =>
                        checked
                          ? [...new Set([...previous, account.id])].slice(
                              0,
                              MAX_BATCH_ACCOUNTS,
                            )
                          : previous.filter((id) => id !== account.id),
                      )
                    }
                  />
                ),
                actions: (
                  <AccountActions
                    account={account}
                    actions={actions}
                    disabled={busy}
                    onConnect={() => setEditor({ kind: "connect", account })}
                    onEdit={() => setEditor({ kind: "edit", account })}
                    onDelete={() => setEditor({ kind: "delete", account })}
                  />
                ),
                footer: hasFeedback(account, actions) ? (
                  <AccountFeedback account={account} actions={actions} />
                ) : undefined,
              })}
              onLoadMore={() => void loadMoreAccounts()}
              onRetry={() => void refresh()}
              state={accounts}
            />
          </TabsContent>
          <TabsContent
            className="gutter-scroller min-h-0 min-w-0 flex-1 overflow-y-auto"
            data-tab-scroller
            forceMount
            hidden={tab !== "records"}
            value="records"
          >
            <JobList
              decorate={(job) => <RecordActions job={job} actions={actions} />}
              onLoadMore={() => void loadMoreJobs()}
              onRetry={() => void refresh()}
              state={{ ...jobs, items: actions.records }}
            />
          </TabsContent>
        </Tabs>
      )}

      {listsReady && editor?.kind === "edit" ? (
        <AccountDialog
          key={editor.account?.id ?? "new"}
          account={editor.account}
          services={services}
          servicesReady={servicesReady}
          onClose={() => setEditor(null)}
          onSaved={accountSaved}
        />
      ) : null}
      {listsReady && editor?.kind === "connect" ? (
        <ConnectAccountDialog
          key={editor.account.id}
          account={editor.account}
          onClose={() => setEditor(null)}
          onConnected={accountSaved}
        />
      ) : null}
      {listsReady && editor?.kind === "delete" ? (
        <DeleteAccountDialog
          key={editor.account.id}
          account={editor.account}
          onClose={() => setEditor(null)}
          onDeleted={accountDeleted}
        />
      ) : null}

      <ConfirmDialog
        cancelLabel={t("enable.cancel")}
        confirmLabel={t("enable.confirmDisable")}
        description={<p>{t("enable.confirmDisableBody")}</p>}
        destructive
        onCancel={() => setConfirmDisable(false)}
        onConfirm={() => {
          setConfirmDisable(false);
          void setEnabled(false);
        }}
        open={confirmDisable}
        title={t("enable.confirmDisableTitle")}
      />
    </section>
  );
}

function initErrorText(t: ReturnType<typeof useCheckinT>, code: string) {
  return checkinHasKey(`initError.${code}`)
    ? t(`initError.${code}`)
    : t("initError.unknown", { code });
}
