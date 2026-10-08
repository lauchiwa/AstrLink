import {
  ArrowUpRight,
  Link as LinkIcon,
  RefreshCw,
  Shredder,
  SquarePen,
} from "@/components/icons";
import { IconButton } from "@/components/IconButton";
import { Panel } from "@/components/Panel";
import { StatusBadge } from "@/components/StatusBadge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Label } from "@/components/ui/label";

import { BatchTally, hostOf, jobDetails, outcomeTones } from "./CheckinLists";
import { useCheckinT } from "./i18n";
import {
  isTerminal,
  jobOutcome,
  type CheckinAccount,
  type CheckinJob,
  type JobAction,
} from "./model";
import {
  BATCH_SLOT,
  MAX_BATCH_ACCOUNTS,
  canCheckIn,
  canRefresh,
  isCancellable,
  readSlot,
  type ActionMessage,
  type CheckinActions,
} from "./use-checkin-actions";

export { useCheckinActions, MAX_BATCH_ACCOUNTS } from "./use-checkin-actions";

const hintID = (account: CheckinAccount) => `checkin-gated-${account.id}`;

export function AccountSelect({
  account,
  checked,
  disabled,
  onCheckedChange,
}: {
  account: CheckinAccount;
  checked: boolean;
  disabled: boolean;
  onCheckedChange: (checked: boolean) => void;
}) {
  const t = useCheckinT();
  return (
    <Checkbox
      aria-label={t("batch.selectAccount", {
        host: hostOf(account.dashboard_base_url),
      })}
      checked={checked}
      disabled={disabled}
      onCheckedChange={(value) => onCheckedChange(value === true)}
    />
  );
}

/** Existing account management stays directly accessible, including its labels. */
export function AccountActions({
  account,
  actions,
  disabled,
  onConnect,
  onEdit,
  onDelete,
}: {
  account: CheckinAccount;
  actions: CheckinActions;
  disabled: boolean;
  onConnect: () => void;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const t = useCheckinT();
  const host = hostOf(account.dashboard_base_url);
  return (
    <>
      {canCheckIn(account) ? (
        <Button
          size="sm"
          variant="outline"
          disabled={disabled || !actions.canRun("check_in", account.id)}
          onClick={() => actions.run("check_in", [account.id])}
        >
          {t("actions.checkIn")}
        </Button>
      ) : (
        <Button
          size="sm"
          variant="outline"
          // A site needing manual action cannot be fixed by another session,
          // so only that state still sends the operator to the site itself.
          disabled={
            disabled ||
            account.state === "manual_required" ||
            actions.accountBusy(account.id)
          }
          aria-describedby={hintID(account)}
          onClick={onConnect}
        >
          {account.state === "manual_required" ? (
            <ArrowUpRight />
          ) : (
            <LinkIcon />
          )}
          {t(
            account.state === "manual_required"
              ? "actions.openSite"
              : "actions.connect",
          )}
        </Button>
      )}
      <IconButton
        label={t("actions.refreshAccount", { host })}
        disabled={disabled || !actions.canRun("status_refresh", account.id)}
        onClick={() => actions.run("status_refresh", [account.id])}
      >
        <RefreshCw />
      </IconButton>
      <IconButton
        label={t("dialog.editAccount", { host })}
        disabled={disabled || actions.accountBusy(account.id)}
        onClick={onEdit}
      >
        <SquarePen />
      </IconButton>
      <IconButton
        label={t("dialog.deleteAccount", { host })}
        disabled={disabled || actions.accountBusy(account.id)}
        onClick={onDelete}
      >
        <Shredder />
      </IconButton>
    </>
  );
}

function MessageLine({
  message,
  onRetry,
}: {
  message: ActionMessage;
  onRetry?: () => void;
}) {
  const t = useCheckinT();
  return (
    <div className="flex min-w-0 flex-wrap items-center gap-2">
      <span
        className={
          message.tone === "error" ? "text-danger-foreground" : undefined
        }
        role={message.tone === "error" ? "alert" : "status"}
      >
        {message.text}
      </span>
      {message.retry && onRetry ? (
        <Button size="xs" variant="ghost" onClick={onRetry}>
          {t("actions.retryReceipt")}
        </Button>
      ) : null}
    </div>
  );
}

export function hasFeedback(
  account: CheckinAccount,
  actions: CheckinActions,
): boolean {
  return (
    !canCheckIn(account) ||
    !!actions.latest(account.id) ||
    !!actions.message(account.id)
  );
}

export function AccountFeedback({
  account,
  actions,
}: {
  account: CheckinAccount;
  actions: CheckinActions;
}) {
  const t = useCheckinT();
  const job = actions.latest(account.id);
  const message = actions.message(account.id);
  const readError = job && actions.message(readSlot(job.id));
  return (
    <div className="grid min-w-0 gap-1">
      {job ? <JobFeedback actions={actions} job={job} /> : null}
      {message ? (
        <MessageLine
          message={message}
          onRetry={() => actions.retry(account.id)}
        />
      ) : null}
      {readError ? <MessageLine message={readError} /> : null}
      {!canCheckIn(account) ? (
        <p id={hintID(account)}>
          {t(
            account.state === "manual_required"
              ? "gated.manual"
              : "gated.connect",
          )}
        </p>
      ) : null}
    </div>
  );
}

function JobFeedback({
  actions,
  job,
}: {
  actions: CheckinActions;
  job: CheckinJob;
}) {
  const t = useCheckinT();
  const outcome = jobOutcome(job);
  const live = !isTerminal(job.status);
  const text = live
    ? [
        t(`status.${job.status}`),
        t(job.dispatched ? "actions.sentPending" : "actions.queued"),
      ].join(" · ")
    : job.status === "cancelled" && !job.dispatched
      ? t("actions.cancelledBeforeSend")
      : [
          ...jobDetails(t, job),
          ...(outcome === "uncertain" ? [t("actions.refreshToConfirm")] : []),
        ].join(" · ");
  return (
    <div className="flex min-w-0 flex-wrap items-center gap-2">
      <StatusBadge tone={outcomeTones[outcome]}>
        {t(`action.${job.action}`)} · {t(`outcome.${outcome}`)}
      </StatusBadge>
      <span className="min-w-0" role="status">
        {text}
      </span>
      <JobControls actions={actions} job={job} />
    </div>
  );
}

function JobControls({
  actions,
  job,
}: {
  actions: CheckinActions;
  job: CheckinJob;
}) {
  const t = useCheckinT();
  const cancelling = actions.cancelling(job.id);
  return (
    <>
      {isCancellable(job) ? (
        <Button
          size="xs"
          variant="ghost"
          disabled={cancelling}
          onClick={() => actions.cancel(job)}
        >
          {t(
            cancelling
              ? "actions.cancelling"
              : job.account_id === undefined
                ? "batch.cancel"
                : "actions.cancel",
          )}
        </Button>
      ) : null}
      {jobOutcome(job) === "uncertain" &&
      job.account_id &&
      actions.canRun("status_refresh", job.account_id) ? (
        <Button
          size="xs"
          variant="ghost"
          onClick={() => actions.run("status_refresh", [job.account_id!])}
        >
          <RefreshCw />
          {t("actions.statusRefresh")}
        </Button>
      ) : null}
    </>
  );
}

/** Controls for persisted tasks, including tasks created before opening this page. */
export function RecordActions({
  job,
  actions,
}: {
  job: CheckinJob;
  actions: CheckinActions;
}) {
  const t = useCheckinT();
  const error = actions.message(readSlot(job.id));
  const slot = job.account_id ?? BATCH_SLOT;
  const message = actions.message(slot);
  return (
    <div className="grid min-w-0 gap-1 text-xs">
      {!isTerminal(job.status) ? (
        <p>{t(job.dispatched ? "actions.sentPending" : "actions.queued")}</p>
      ) : null}
      {jobOutcome(job) === "uncertain" ? (
        <p>{t("actions.refreshToConfirm")}</p>
      ) : null}
      <div className="flex flex-wrap items-center gap-2">
        <JobControls actions={actions} job={job} />
      </div>
      {error ? <MessageLine message={error} /> : null}
      {message ? (
        <MessageLine message={message} onRetry={() => actions.retry(slot)} />
      ) : null}
    </div>
  );
}

export function batchEligible(
  accounts: readonly CheckinAccount[],
  actions: CheckinActions,
): string[] {
  return accounts
    .filter(
      (account) =>
        canRefresh(account) && actions.canRun("status_refresh", account.id),
    )
    .map((account) => account.id);
}

export function BatchToolbar({
  actions,
  eligible,
  selection,
  disabled,
  onSelectionChange,
}: {
  actions: CheckinActions;
  eligible: readonly string[];
  selection: readonly string[];
  disabled: boolean;
  onSelectionChange: (selection: string[]) => void;
}) {
  const t = useCheckinT();
  const capped = eligible.slice(0, MAX_BATCH_ACCOUNTS);
  const all = capped.length > 0 && capped.every((id) => selection.includes(id));
  const start = (action: JobAction) => {
    if (actions.run(action, selection)) onSelectionChange([]);
  };
  const unavailable = (action: JobAction) =>
    disabled ||
    !selection.length ||
    actions.batchBusy ||
    selection.some((id) => !actions.canRun(action, id));
  return (
    <div
      className="flex min-w-0 flex-wrap items-center gap-2"
      data-testid="checkin-batch-toolbar"
    >
      <Label
        className="font-normal text-muted-foreground tabular-nums"
        title={t("batch.limitHint", { limit: MAX_BATCH_ACCOUNTS })}
      >
        <Checkbox
          aria-label={t("batch.selectAll")}
          checked={all ? true : selection.length ? "indeterminate" : false}
          disabled={disabled || !eligible.length}
          onCheckedChange={(value) =>
            onSelectionChange(value === true ? capped : [])
          }
        />
        {t("batch.selected", {
          selected: selection.length,
          limit: MAX_BATCH_ACCOUNTS,
        })}
      </Label>
      <Button
        size="sm"
        variant="ghost"
        disabled={unavailable("status_refresh")}
        onClick={() => start("status_refresh")}
      >
        {t("batch.statusRefresh")}
      </Button>
      <Button
        size="sm"
        variant="outline"
        disabled={unavailable("check_in")}
        onClick={() => start("check_in")}
      >
        {t("batch.checkIn")}
      </Button>
    </div>
  );
}

/** Kept inside the panel scroller, so long feedback never consumes the toolbar. */
export function BatchSummary({ actions }: { actions: CheckinActions }) {
  const t = useCheckinT();
  const job = actions.latestBatch;
  const message = actions.message(BATCH_SLOT);
  if (!job && !message) return null;
  const error = job && actions.message(readSlot(job.id));
  return (
    <Panel
      className="mb-3 flex min-w-0 flex-wrap items-center gap-2 px-3 py-2 text-xs"
      tone="inset"
      data-testid="checkin-batch-summary"
    >
      {job ? (
        <>
          <span className="font-medium">
            {t("batch.summary", {
              action: t(`action.${job.action}`),
              total: job.children.length,
            })}
          </span>
          <BatchTally
            jobs={job.children.map(
              (child) => actions.receipt(child.id) ?? child,
            )}
          />
          <JobControls job={job} actions={actions} />
        </>
      ) : null}
      {message ? (
        <MessageLine
          message={message}
          onRetry={() => actions.retry(BATCH_SLOT)}
        />
      ) : null}
      {error ? <MessageLine message={error} /> : null}
    </Panel>
  );
}
