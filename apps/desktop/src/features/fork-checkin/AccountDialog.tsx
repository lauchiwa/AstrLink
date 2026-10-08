import { useId, useRef, useState } from "react";

import { ConfirmDialog } from "@/components/ConfirmDialog";
import { Field } from "@/components/Field";
import { FilterSelect } from "@/components/FilterSelect";
import { FormMessage } from "@/components/FormMessage";
import { MultiFilterSelect } from "@/components/MultiFilterSelect";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import type { Service } from "../../service-model";

import {
  accountDraft,
  createAccountInput,
  draftError,
  MAX_BOUND_SERVICES,
  needsTargetConfirmation,
  redirectsAccount,
  updateAccountInput,
  type AccountDraft,
} from "./account-draft";
import {
  CheckinBridgeError,
  createCheckinAccount,
  describeCheckinError,
  getCheckinAccount,
  updateCheckinAccount,
  type CheckinResume,
} from "./bridge";
import { useCheckinT } from "./i18n";
import type { CheckinAccount, NetworkMode } from "./model";
import { useAccountRequest } from "./use-account-request";

export interface CheckinServiceCatalog {
  services?: readonly Pick<Service, "id" | "name">[];
  servicesReady?: boolean;
}

/** No authentication material enters this editor; connection is a native task. */
export function AccountDialog({
  account,
  services = [],
  servicesReady = false,
  onClose,
  onSaved,
}: CheckinServiceCatalog & {
  account: CheckinAccount | null;
  onClose: () => void;
  onSaved: (account: CheckinAccount) => void;
}) {
  const t = useCheckinT();
  const id = useId();
  const [base, setBase] = useState(account);
  const [draft, setDraft] = useState(() => accountDraft(account));
  const [message, setMessage] = useState<{
    error: boolean;
    text: string;
  } | null>(null);
  const [conflict, setConflict] = useState(false);
  const [gone, setGone] = useState(false);
  const [approveAutomatic, setApproveAutomatic] = useState(false);
  const resume = useRef<CheckinResume | null>(null);
  const request = useAccountRequest();
  const redirected = redirectsAccount(draft, base);
  const canAutomate = base?.state === "connected" && !redirected;
  const mustConfirm = needsTargetConfirmation(draft, base);
  const serviceNames = new Map(
    services.map((service) => [service.id, service.name]),
  );
  // Keep removed bindings visible. They never disappear just because a catalog
  // refresh failed, or because a provider was renamed or deleted elsewhere.
  const serviceOptions = [
    ...new Set([
      ...serviceNames.keys(),
      ...(base?.bound_services ?? []),
      ...draft.boundServices,
    ]),
  ].map((value) => ({
    value,
    label:
      serviceNames.get(value) ??
      (servicesReady ? t("dialog.removedService", { id: value }) : value),
  }));

  const change = (patch: Partial<AccountDraft>, target = false) => {
    setDraft((previous) => ({
      ...previous,
      ...patch,
      ...(target ? { targetConfirmed: false } : {}),
    }));
    setMessage(null);
  };
  const failed = (error: unknown, keepConflict = false) => {
    resume.current = error instanceof CheckinBridgeError ? error.resume : null;
    setConflict(
      (previous) =>
        (keepConflict && previous) ||
        (error instanceof CheckinBridgeError &&
          error.code === "revision_conflict"),
    );
    setGone(error instanceof CheckinBridgeError && error.code === "not_found");
    setMessage({ error: true, text: describeCheckinError(error) });
  };
  const reload = () => {
    if (!base) return;
    void request.run(
      () => getCheckinAccount(base.id),
      (latest) => {
        setBase(latest);
        setDraft(accountDraft(latest));
        resume.current = null;
        setConflict(false);
        setGone(false);
        setMessage({ error: false, text: t("dialog.reloaded") });
      },
      (error) => failed(error, true),
    );
  };
  const save = (automaticApproved = false) => {
    if (request.busy || conflict || gone) return;
    const error = draftError(draft, base);
    if (error) {
      setMessage({
        error: true,
        text: t(`validation.${error}`, { limit: MAX_BOUND_SERVICES }),
      });
      return;
    }
    const bindingsChanged =
      base &&
      JSON.stringify([...draft.boundServices].sort()) !==
        JSON.stringify([...base.bound_services].sort());
    if (bindingsChanged && !servicesReady) {
      setMessage({ error: true, text: t("dialog.servicesUnavailable") });
      return;
    }
    if (
      bindingsChanged &&
      draft.boundServices.some(
        (value) =>
          !serviceNames.has(value) && !base.bound_services.includes(value),
      )
    ) {
      setMessage({ error: true, text: t("dialog.servicesChanged") });
      return;
    }
    const patch = base ? updateAccountInput(draft, base) : null;
    if (base && !patch) {
      setMessage({ error: false, text: t("dialog.unchanged") });
      return;
    }
    if (patch?.automatic === true && !automaticApproved) {
      setApproveAutomatic(true);
      return;
    }
    setMessage(null);
    void request.run(
      () =>
        base && patch
          ? updateCheckinAccount(base.id, patch, resume.current)
          : createCheckinAccount(createAccountInput(draft), resume.current),
      onSaved,
      failed,
    );
  };

  return (
    <>
      <Dialog
        open
        onOpenChange={(open) => {
          if (!open && !request.isPending()) onClose();
        }}
      >
        <DialogContent
          variant="workspace"
          className="sm:max-w-2xl"
          showCloseButton={!request.busy}
          onInteractOutside={(event) => event.preventDefault()}
          onEscapeKeyDown={(event) => {
            if (request.isPending()) event.preventDefault();
          }}
        >
          <DialogHeader className="shrink-0 border-b p-4 pr-10">
            <DialogTitle>
              {t(base ? "dialog.editTitle" : "dialog.createTitle")}
            </DialogTitle>
            <DialogDescription>{t("dialog.description")}</DialogDescription>
          </DialogHeader>
          <div
            className="min-h-0 flex-1 overflow-y-auto p-4"
            data-testid="checkin-account-editor-body"
          >
            <div className="grid gap-4 sm:grid-cols-2">
              <Field
                className="sm:col-span-2"
                label={t("dialog.dashboardURL")}
                hint={t("dialog.dashboardURLHint")}
                htmlFor={`${id}-url`}
              >
                <Input
                  aria-label={t("dialog.dashboardURL")}
                  id={`${id}-url`}
                  value={draft.dashboardURL}
                  disabled={request.busy}
                  autoComplete="off"
                  spellCheck={false}
                  onChange={(event) =>
                    change({ dashboardURL: event.target.value }, true)
                  }
                />
              </Field>
              <Field
                label={t("dialog.timeZone")}
                hint={t("dialog.timeZoneHint")}
                htmlFor={`${id}-zone`}
              >
                <Input
                  aria-label={t("dialog.timeZone")}
                  id={`${id}-zone`}
                  value={draft.timeZone}
                  disabled={request.busy}
                  autoComplete="off"
                  spellCheck={false}
                  onChange={(event) =>
                    change({ timeZone: event.target.value }, true)
                  }
                />
              </Field>
              <Field label={t("dialog.network")} group>
                <FilterSelect
                  ariaLabel={t("dialog.network")}
                  label=""
                  disabled={request.busy}
                  value={draft.networkMode}
                  onChange={(mode) =>
                    change({ networkMode: mode as NetworkMode }, true)
                  }
                  options={(["direct", "system", "custom"] as const).map(
                    (mode) => ({ value: mode, label: t(`network.${mode}`) }),
                  )}
                />
              </Field>
              {draft.networkMode === "custom" ? (
                <Field
                  className="sm:col-span-2"
                  label={t("dialog.proxyURL")}
                  hint={t("dialog.proxyURLHint")}
                  htmlFor={`${id}-proxy`}
                >
                  <Input
                    aria-label={t("dialog.proxyURL")}
                    id={`${id}-proxy`}
                    value={draft.proxyURL}
                    disabled={request.busy}
                    autoComplete="off"
                    spellCheck={false}
                    onChange={(event) =>
                      change({ proxyURL: event.target.value }, true)
                    }
                  />
                </Field>
              ) : null}
              {base ? (
                <Field
                  className="sm:col-span-2"
                  label={t("dialog.boundServices")}
                  hint={t(
                    servicesReady
                      ? "dialog.boundServicesHint"
                      : "dialog.servicesUnavailable",
                  )}
                  group
                >
                  <MultiFilterSelect
                    allLabel={t("accounts.noBoundServices")}
                    ariaLabel={t("dialog.boundServices")}
                    className="w-full"
                    contentClassName="z-110"
                    label=""
                    disabled={request.busy || !servicesReady}
                    clearLabel={t("dialog.clearServices")}
                    selectAllLabel={t("dialog.selectAllServices")}
                    searchPlaceholder={t("dialog.searchServices")}
                    emptyMessage={t("dialog.noServices")}
                    selectedCountLabel={(total) =>
                      t("accounts.boundServices", { total })
                    }
                    options={serviceOptions}
                    value={draft.boundServices}
                    onChange={(boundServices) => change({ boundServices })}
                  />
                </Field>
              ) : (
                <p className="text-xs text-muted-foreground sm:col-span-2">
                  {t("dialog.bindAfterCreate")}
                </p>
              )}
              <div className="grid gap-1.5 sm:col-span-2">
                <Label
                  className="flex items-center gap-2"
                  htmlFor={`${id}-automatic`}
                >
                  <Checkbox
                    id={`${id}-automatic`}
                    checked={canAutomate && draft.automatic}
                    disabled={request.busy || !canAutomate}
                    onCheckedChange={(checked) =>
                      change({ automatic: checked === true })
                    }
                  />
                  {t("dialog.automatic")}
                </Label>
                <p className="text-xs text-muted-foreground">
                  {t("dialog.automaticHint")}
                </p>
              </div>
              {redirected ? (
                <FormMessage className="sm:col-span-2" tone="notice">
                  {t("dialog.redirectWarning")}
                </FormMessage>
              ) : null}
              {mustConfirm ? (
                <Label
                  className="flex items-start gap-2 sm:col-span-2"
                  htmlFor={`${id}-target`}
                >
                  <Checkbox
                    id={`${id}-target`}
                    checked={draft.targetConfirmed}
                    disabled={request.busy}
                    onCheckedChange={(checked) =>
                      change({ targetConfirmed: checked === true })
                    }
                  />
                  {t("dialog.confirmTarget")}
                </Label>
              ) : null}
              <div className="grid gap-1.5 sm:col-span-2">
                <Button
                  className="justify-self-start"
                  variant="outline"
                  size="sm"
                  disabled
                  aria-describedby={`${id}-gated`}
                >
                  {t("actions.connect")}
                </Button>
                <p id={`${id}-gated`} className="text-xs text-muted-foreground">
                  {t("gated.connect")}
                </p>
              </div>
              {message ? (
                <FormMessage
                  className="sm:col-span-2"
                  tone={message.error ? "error" : "notice"}
                >
                  {message.text}
                </FormMessage>
              ) : null}
              {conflict ? (
                <Button
                  className="justify-self-start"
                  variant="outline"
                  disabled={request.busy}
                  onClick={reload}
                >
                  {t("dialog.reload")}
                </Button>
              ) : null}
            </div>
          </div>
          <DialogFooter className="shrink-0 border-t p-4">
            <Button
              variant="outline"
              disabled={request.busy}
              onClick={() => {
                if (!request.isPending()) onClose();
              }}
            >
              {t("dialog.cancel")}
            </Button>
            <Button
              disabled={request.busy || conflict || gone}
              onClick={() => save()}
            >
              {t(
                base
                  ? request.busy
                    ? "dialog.saving"
                    : "dialog.save"
                  : request.busy
                    ? "dialog.creating"
                    : "dialog.create",
              )}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <ConfirmDialog
        open={approveAutomatic}
        title={t("dialog.automaticTitle")}
        description={
          <>
            <p>{t("dialog.automaticBody")}</p>
            <p>{t("dialog.automaticFeature")}</p>
          </>
        }
        confirmLabel={t("dialog.automaticConfirm")}
        cancelLabel={t("dialog.automaticCancel")}
        onCancel={() => setApproveAutomatic(false)}
        onConfirm={() => {
          setApproveAutomatic(false);
          save(true);
        }}
      />
    </>
  );
}
