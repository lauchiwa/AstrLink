import type { ReactNode } from "react";

import { DataRow } from "@/components/DataRow";
import { EmptyState } from "@/components/EmptyState";
import { FormMessage } from "@/components/FormMessage";
import { LoadingState } from "@/components/LoadingState";
import { Panel } from "@/components/Panel";
import { StatusBadge } from "@/components/StatusBadge";
import type { StatusTone } from "@/components/StatusDot";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { i18n } from "@/i18n";

import { checkinHasKey, useCheckinT } from "./i18n";
import {
  jobOutcome,
  type AccountState,
  type CheckinAccount,
  type CheckinJob,
  type JobOutcome,
} from "./model";

/** One cursor-paged list as the workspace holds it. */
export interface ListState<T> {
  status: "idle" | "loading" | "ready" | "error";
  items: T[];
  cursor?: string;
  error: string | null;
  loadingMore: boolean;
}

const accountTones: Record<AccountState, StatusTone> = {
  draft: "neutral",
  connected: "positive",
  auth_required: "blocked",
  manual_required: "blocked",
};

/** Controls the workspace attaches to one account row. */
export interface AccountRowDecoration {
  leading?: ReactNode;
  actions?: ReactNode;
  footer?: ReactNode;
}

export const outcomeTones: Record<JobOutcome, StatusTone> = {
  done: "positive",
  pending: "pending",
  needs_operator: "blocked",
  uncertain: "pending",
  not_done: "neutral",
  failed: "negative",
};

const outcomeOrder: readonly JobOutcome[] = [
  "done",
  "pending",
  "needs_operator",
  "uncertain",
  "not_done",
  "failed",
];

export function hostOf(address: string): string {
  try {
    return new URL(address).host;
  } catch {
    return address;
  }
}

export function formatTime(value: string): string {
  return new Intl.DateTimeFormat(i18n.language, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(new Date(value));
}

export function failureText(
  t: ReturnType<typeof useCheckinT>,
  code: string,
): string {
  return checkinHasKey(`failure.${code}`)
    ? t(`failure.${code}`)
    : t("failure.unknown", { code });
}

/**
 * What a receipt says, in order: its exact status, the site's date, the
 * reward only when the site reported one, the reason and whether a
 * submission left the machine. Nothing is inferred or estimated.
 */
export function jobDetails(
  t: ReturnType<typeof useCheckinT>,
  job: CheckinJob,
): string[] {
  const outcome = jobOutcome(job);
  const batch = job.account_id === undefined;
  const details: string[] = [t(`status.${job.status}`)];
  if (job.site_date) details.push(t("jobs.siteDate", { date: job.site_date }));
  if (outcome === "done" && !batch) {
    details.push(
      job.reward?.known
        ? t("jobs.rewardKnown", {
            quota: job.reward.quota,
            unit: job.reward.unit,
          })
        : t("jobs.rewardUnknown"),
    );
  }
  if (job.failure_code) {
    details.push(
      t("jobs.failure", { reason: failureText(t, job.failure_code) }),
    );
  }
  if (job.dispatched && outcome !== "done") details.push(t("jobs.dispatched"));
  return details;
}

/** Shared states around a paged list; the caller renders the rows. */
function ListBody<T>({
  emptyHint,
  emptyTitle,
  label,
  loadingLabel,
  onLoadMore,
  onRetry,
  rows,
  state,
}: {
  emptyHint: string;
  emptyTitle: string;
  label: string;
  loadingLabel: string;
  onLoadMore: () => void;
  onRetry: () => void;
  rows: (items: T[]) => ReactNode;
  state: ListState<T>;
}) {
  const t = useCheckinT();
  if (state.status === "error" && state.items.length === 0) {
    return (
      <EmptyState
        action={
          <Button onClick={onRetry} size="sm" type="button" variant="outline">
            {t("workspace.retry")}
          </Button>
        }
        description={state.error ?? t("workspace.readFailedHint")}
        title={t("workspace.readFailed")}
      />
    );
  }
  if (state.items.length === 0 && state.status !== "ready") {
    return (
      <div className="flex justify-center py-10">
        <LoadingState label={loadingLabel} />
      </div>
    );
  }
  if (state.items.length === 0) {
    return <EmptyState description={emptyHint} title={emptyTitle} />;
  }
  return (
    <div className="grid gap-3">
      {state.error ? (
        <FormMessage tone="error">{state.error}</FormMessage>
      ) : null}
      <Panel aria-label={label} role="list">
        {rows(state.items)}
      </Panel>
      {state.cursor ? (
        <div className="flex justify-center">
          <Button
            disabled={state.loadingMore}
            onClick={onLoadMore}
            size="sm"
            type="button"
            variant="outline"
          >
            {state.loadingMore
              ? t("workspace.loadingMore")
              : t("workspace.loadMore")}
          </Button>
        </div>
      ) : null}
    </div>
  );
}

function AccountRow({
  account,
  decoration = {},
}: {
  account: CheckinAccount;
  decoration?: AccountRowDecoration;
}) {
  const t = useCheckinT();
  const host = hostOf(account.dashboard_base_url);
  return (
    <DataRow asChild className="flex-col items-stretch gap-1.5">
      <article data-testid="checkin-account-row" role="listitem">
        <div className="flex min-w-0 flex-wrap items-center gap-3">
          {decoration.leading ? (
            <span className="flex shrink-0 items-center">
              {decoration.leading}
            </span>
          ) : null}
          <div className="grid min-w-0 flex-1 basis-40 gap-1">
            <strong
              className="truncate text-sm font-semibold"
              title={account.dashboard_base_url}
            >
              {host}
            </strong>
            <span className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
              <Badge variant="outline">
                {t(
                  account.automatic
                    ? "accounts.automaticOn"
                    : "accounts.automaticOff",
                )}
              </Badge>
              <StatusBadge tone={accountTones[account.state]}>
                {t(`accountState.${account.state}`)}
              </StatusBadge>
              <span>{t(`network.${account.network.mode}`)}</span>
              <span>{account.time_zone}</span>
              {account.remote_user_id ? (
                <span className="truncate" title={account.remote_user_id}>
                  {t("accounts.remoteUser", { id: account.remote_user_id })}
                </span>
              ) : null}
              <span>
                {account.bound_services.length > 0
                  ? t("accounts.boundServices", {
                      total: account.bound_services.length,
                    })
                  : t("accounts.noBoundServices")}
              </span>
            </span>
          </div>
          <div className="ml-auto flex shrink-0 flex-wrap items-center justify-end gap-2">
            {decoration.actions}
          </div>
        </div>
        {decoration.footer ? (
          <div
            className="min-w-0 text-xs text-muted-foreground"
            data-testid="checkin-account-feedback"
          >
            {decoration.footer}
          </div>
        ) : null}
      </article>
    </DataRow>
  );
}

/** Each child's outcome counted on its own; a parent never hides a failure. */
export function BatchTally({ jobs }: { jobs: CheckinJob[] }) {
  const t = useCheckinT();
  const counts = new Map<JobOutcome, number>();
  for (const job of jobs) {
    const outcome = jobOutcome(job);
    counts.set(outcome, (counts.get(outcome) ?? 0) + 1);
  }
  return (
    <span className="flex flex-wrap gap-1.5">
      {outcomeOrder
        .filter((outcome) => counts.has(outcome))
        .map((outcome) => (
          <StatusBadge key={outcome} tone={outcomeTones[outcome]}>
            {t(`outcome.${outcome}`)} · {counts.get(outcome)}
          </StatusBadge>
        ))}
    </span>
  );
}

function JobRow({
  job,
  decoration,
}: {
  job: CheckinJob;
  decoration?: ReactNode;
}) {
  const t = useCheckinT();
  const outcome = jobOutcome(job);
  const batch = job.account_id === undefined;
  const details = jobDetails(t, job);

  return (
    <DataRow asChild className="grid grid-cols-[minmax(0,1fr)_auto] gap-3">
      <article data-testid="checkin-job-row" role="listitem">
        <div className="grid min-w-0 gap-1">
          <strong className="flex min-w-0 items-center gap-2 text-sm font-semibold">
            <span className="truncate">{t(`action.${job.action}`)}</span>
            <span className="truncate text-xs font-normal text-muted-foreground">
              {batch
                ? t("jobs.batch", { total: job.children.length })
                : t("jobs.account", { id: job.account_id })}
            </span>
          </strong>
          <span className="text-xs text-muted-foreground">
            {details.join(" · ")}
          </span>
          {batch && job.children.length > 0 ? (
            <BatchTally jobs={job.children} />
          ) : null}
          <span className="text-micro tabular-nums text-muted-foreground">
            {job.finished_at
              ? t("jobs.finishedAt", { time: formatTime(job.finished_at) })
              : t("jobs.createdAt", { time: formatTime(job.created_at) })}
          </span>
        </div>
        <StatusBadge className="shrink-0" tone={outcomeTones[outcome]}>
          {t(`outcome.${outcome}`)}
        </StatusBadge>
        {decoration ? (
          <div className="col-span-2 min-w-0">{decoration}</div>
        ) : null}
      </article>
    </DataRow>
  );
}

export function AccountList({
  decorate,
  onLoadMore,
  onRetry,
  state,
}: {
  decorate?: (account: CheckinAccount) => AccountRowDecoration;
  onLoadMore: () => void;
  onRetry: () => void;
  state: ListState<CheckinAccount>;
}) {
  const t = useCheckinT();
  return (
    <ListBody
      emptyHint={t("accounts.emptyHint")}
      emptyTitle={t("accounts.empty")}
      label={t("accounts.listLabel")}
      loadingLabel={t("workspace.loading")}
      onLoadMore={onLoadMore}
      onRetry={onRetry}
      rows={(items) =>
        items.map((account) => (
          <AccountRow
            account={account}
            decoration={decorate?.(account)}
            key={account.id}
          />
        ))
      }
      state={state}
    />
  );
}

export function JobList({
  decorate,
  onLoadMore,
  onRetry,
  state,
}: {
  decorate?: (job: CheckinJob) => ReactNode;
  onLoadMore: () => void;
  onRetry: () => void;
  state: ListState<CheckinJob>;
}) {
  const t = useCheckinT();
  return (
    <ListBody
      emptyHint={t("jobs.emptyHint")}
      emptyTitle={t("jobs.empty")}
      label={t("jobs.listLabel")}
      loadingLabel={t("workspace.loading")}
      onLoadMore={onLoadMore}
      onRetry={onRetry}
      rows={(items) =>
        items.map((job) => (
          <JobRow job={job} decoration={decorate?.(job)} key={job.id} />
        ))
      }
      state={state}
    />
  );
}
