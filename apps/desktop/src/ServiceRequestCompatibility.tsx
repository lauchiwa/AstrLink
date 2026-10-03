import { useCallback, useEffect, useRef, useState } from "react";
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
import {
  identityClientLabels,
  identityClients,
  type CompatibilityDraft,
  type IdentityClient,
  type IdentityCaptureStatus,
  type IdentityProfile,
  type IdentityProfileRecord,
} from "./request-compatibility-model";
import { useT } from "./i18n";
import { ConfirmDialog } from "./components/ConfirmDialog";
import { EmptyState } from "./components/EmptyState";
import { Field } from "./components/Field";
import { FormMessage } from "./components/FormMessage";
import { Panel } from "./components/Panel";
import { RequestRulesEditor } from "./components/RequestRulesEditor";
import { Button } from "./components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "./components/ui/dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "./components/ui/select";

export function ServiceRequestCompatibility({
  serviceId,
  value,
  onChange,
}: {
  serviceId?: string;
  value: CompatibilityDraft;
  onChange: (value: CompatibilityDraft) => void;
}) {
  const t = useT();
  const [profiles, setProfiles] = useState<IdentityProfile[]>([]);
  const [capture, setCapture] = useState<IdentityCaptureStatus | null>(null);
  const [review, setReview] = useState<IdentityProfileRecord | null>(null);
  const [open, setOpen] = useState(false);
  const [client, setClient] = useState<IdentityClient>("codex_cli");
  const [source, setSource] = useState<"builtin" | "subscription_import">(
    "builtin",
  );
  const [confirm, setConfirm] = useState<
    | { kind: "arm"; client: IdentityClient }
    | { kind: "confirm" | "discard"; record: IdentityProfileRecord }
    | null
  >(null);
  const [busy, setBusy] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const generation = useRef(0);
  const pending = useRef(false);
  const run = useCallback(
    async <T,>(action: () => Promise<T>, apply: (result: T) => void) => {
      if (pending.current) return;
      const current = generation.current;
      pending.current = true;
      setBusy(true);
      setError(null);
      try {
        const result = await action();
        if (current === generation.current) apply(result);
      } catch (cause) {
        if (current === generation.current)
          setError(
            cause instanceof Error ? cause.message : t("compatibility.failed"),
          );
      } finally {
        if (current === generation.current) {
          pending.current = false;
          setBusy(false);
        }
      }
    },
    [t],
  );
  const load = useCallback(async () => {
    if (!serviceId) return;
    await run(
      async () => {
        const items: IdentityProfile[] = [];
        const cursors = new Set<string>();
        let cursor: string | undefined;
        do {
          const page = await listIdentityProfiles(serviceId, cursor);
          items.push(...page.items);
          cursor = page.next_cursor ?? undefined;
          if (cursor && cursors.has(cursor))
            throw new Error(t("compatibility.failed"));
          if (cursor) cursors.add(cursor);
        } while (cursor);
        return { items, capture: await getIdentityCapture(serviceId) };
      },
      ({ items, capture }) => {
        setProfiles(items);
        setCapture(capture);
        setLoaded(true);
      },
    );
  }, [run, serviceId, t]);
  useEffect(() => {
    generation.current++;
    pending.current = false;
    setProfiles([]);
    setCapture(null);
    setReview(null);
    setLoaded(false);
    void load();
    return () => {
      generation.current++;
    };
  }, [load]);
  const received = (record: IdentityProfileRecord) => {
    setReview(record);
    setProfiles((current) => [
      ...current.filter((profile) => profile.id !== record.profile.id),
      record.profile,
    ]);
  };
  useEffect(() => {
    // Never replace the snapshot being reviewed or awaiting consent.
    if (!serviceId || !open || !capture?.armed || review || confirm) return;
    const timer = window.setInterval(() => {
      void run(
        async () => {
          const status = await getIdentityCapture(serviceId);
          const record = status.captured_profile
            ? await getIdentityProfile(serviceId, status.captured_profile)
            : null;
          return { status, record };
        },
        ({ status, record }) => {
          setCapture(status);
          if (record) received(record);
        },
      );
    }, 3000);
    return () => window.clearInterval(timer);
  }, [capture?.armed, confirm, open, review, run, serviceId]);
  const usable = profiles.filter((profile) => profile.confirmed_at);
  const label = (profile: IdentityProfile) =>
    `${identityClientLabels[profile.client]} · ${profile.fingerprint.version} · ${profile.id.slice(-8)}`;
  const selectProfile = (id: string) =>
    onChange({ ...value, identity: id === "none" ? "" : id });
  const handleConfirm = () => {
    const action = confirm;
    setConfirm(null);
    if (!serviceId) return;
    if (action?.kind === "arm")
      void run(() => armIdentityCapture(serviceId, action.client), setCapture);
    if (action?.kind === "confirm")
      void run(
        () => confirmIdentityProfile(serviceId, action.record),
        received,
      );
    if (action?.kind === "discard") {
      const id = action.record.profile.id;
      void run(
        () => discardIdentityProfile(serviceId, action.record),
        () => {
          setReview(null);
          setProfiles((current) =>
            current.filter((profile) => profile.id !== id),
          );
        },
      );
    }
  };
  return (
    <div
      className="flex h-full min-h-0 flex-col gap-2"
      data-testid="request-compatibility"
    >
      <div className="flex shrink-0 flex-wrap items-end gap-2">
        <Field
          className="min-w-0 flex-1 basis-52 flex-row items-center gap-2"
          label={
            <span className="whitespace-nowrap">
              {t("compatibility.identity")}
            </span>
          }
        >
          <Select
            value={value.identity || "none"}
            onValueChange={selectProfile}
            disabled={!serviceId || busy}
          >
            <SelectTrigger aria-label={t("compatibility.identity")}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="none">
                {t("compatibility.noIdentity")}
              </SelectItem>
              {value.identity &&
              !usable.some(({ id }) => id === value.identity) ? (
                <SelectItem value={value.identity}>{value.identity}</SelectItem>
              ) : null}
              {usable.map((profile) => (
                <SelectItem key={profile.id} value={profile.id}>
                  {label(profile)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>
        <Button
          type="button"
          variant="outline"
          disabled={!serviceId}
          onClick={() => {
            setOpen(true);
            void load();
          }}
        >
          {t("compatibility.manage")}
        </Button>
      </div>
      {!serviceId ? (
        <p className="shrink-0 text-xs text-muted-foreground">
          {t("compatibility.saveFirst")}
        </p>
      ) : null}
      {error && !open ? (
        <FormMessage tone="error">
          {error}
          <Button type="button" variant="ghost" onClick={() => void load()}>
            {t("common.retry")}
          </Button>
        </FormMessage>
      ) : null}
      <RequestRulesEditor value={value} onChange={onChange} />
      <Dialog
        open={open}
        onOpenChange={(next) => {
          if (!busy) setOpen(next);
        }}
      >
        <DialogContent
          variant="workspace"
          className="sm:max-w-3xl"
          showCloseButton={!busy}
        >
          <DialogHeader className="shrink-0 border-b p-4 pr-10">
            <DialogTitle>{t("compatibility.manage")}</DialogTitle>
            <DialogDescription>
              {t("compatibility.profileHelp")}
            </DialogDescription>
          </DialogHeader>
          {error ? (
            <div className="px-4 pt-2">
              <FormMessage tone="error">{error}</FormMessage>
            </div>
          ) : null}
          {review ? (
            <>
              <div className="flex shrink-0 flex-wrap items-center gap-2 border-b p-3">
                <Button
                  type="button"
                  variant="ghost"
                  disabled={busy}
                  onClick={() => setReview(null)}
                >
                  {t("compatibility.back")}
                </Button>
                <span className="min-w-0 flex-1 truncate text-sm">
                  {label(review.profile)}
                </span>
                <span className="text-xs text-muted-foreground">
                  {t(
                    review.profile.confirmed_at
                      ? "compatibility.confirmed"
                      : "compatibility.candidate",
                  )}
                </span>
              </div>
              <div
                className="min-h-0 flex-1 overflow-y-auto p-4"
                data-testid="identity-review-region"
              >
                <p className="mb-3 break-all font-mono text-xs">
                  {review.profile.id}
                </p>
                <Panel className="p-3">
                  <pre className="whitespace-pre-wrap break-all font-mono text-xs">
                    {JSON.stringify(review.profile.fingerprint, null, 2)}
                  </pre>
                </Panel>
                <p className="mt-3 text-xs text-muted-foreground">
                  {t("compatibility.reviewHelp")}
                </p>
              </div>
              <div className="flex shrink-0 flex-wrap justify-end gap-2 border-t p-3">
                {review.profile.confirmed_at ? (
                  <Button
                    type="button"
                    disabled={busy}
                    onClick={() => {
                      selectProfile(review.profile.id);
                      setOpen(false);
                    }}
                  >
                    {t("compatibility.useIdentity")}
                  </Button>
                ) : (
                  <>
                    <Button
                      type="button"
                      variant="outline"
                      disabled={busy}
                      onClick={() =>
                        setConfirm({ kind: "discard", record: review })
                      }
                    >
                      {t("compatibility.discard")}
                    </Button>
                    <Button
                      type="button"
                      disabled={busy}
                      onClick={() =>
                        setConfirm({ kind: "confirm", record: review })
                      }
                    >
                      {t("compatibility.confirm")}
                    </Button>
                  </>
                )}
              </div>
            </>
          ) : (
            <>
              <div className="flex shrink-0 flex-wrap items-end gap-2 border-b p-3">
                <Field
                  className="min-w-36 flex-1"
                  label={t("compatibility.client")}
                >
                  <Select
                    value={client}
                    onValueChange={(next) => setClient(next as IdentityClient)}
                    disabled={busy || capture?.armed}
                  >
                    <SelectTrigger aria-label={t("compatibility.client")}>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {identityClients.map((item) => (
                        <SelectItem key={item} value={item}>
                          {identityClientLabels[item]}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </Field>
                <Field
                  className="min-w-40 flex-1"
                  label={t("compatibility.source")}
                >
                  <Select
                    value={source}
                    onValueChange={(next) => setSource(next as typeof source)}
                    disabled={busy}
                  >
                    <SelectTrigger aria-label={t("compatibility.source")}>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="builtin">
                        {t("compatibility.builtin")}
                      </SelectItem>
                      <SelectItem value="subscription_import">
                        {t("compatibility.subscription_import")}
                      </SelectItem>
                    </SelectContent>
                  </Select>
                </Field>
                <Button
                  type="button"
                  disabled={busy || !loaded}
                  onClick={() => {
                    if (serviceId)
                      void run(
                        () => createIdentityProfile(serviceId, client, source),
                        received,
                      );
                  }}
                >
                  {t("compatibility.create")}
                </Button>
              </div>
              <div className="flex shrink-0 flex-wrap items-center gap-2 border-b px-3 py-2">
                <p
                  className="min-w-0 flex-1 text-xs text-muted-foreground"
                  role="status"
                >
                  {capture?.armed
                    ? t("compatibility.capturing", {
                        client: identityClientLabels[capture.client!],
                        expires: new Date(
                          capture.expires_at!,
                        ).toLocaleTimeString(),
                        count: capture.rejected,
                      })
                    : capture?.captured_profile
                      ? t("compatibility.captured")
                      : t("compatibility.captureHint")}
                </p>
                {capture?.armed ? (
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    disabled={busy}
                    onClick={() => {
                      if (serviceId)
                        void run(
                          () => disarmIdentityCapture(serviceId),
                          setCapture,
                        );
                    }}
                  >
                    {t("compatibility.stop")}
                  </Button>
                ) : (
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    disabled={busy || !loaded}
                    onClick={() => setConfirm({ kind: "arm", client })}
                  >
                    {t("compatibility.arm")}
                  </Button>
                )}
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  disabled={busy}
                  onClick={() => void load()}
                >
                  {t("common.refresh")}
                </Button>
              </div>
              <div
                className="min-h-0 flex-1 overflow-y-auto p-3"
                data-testid="identity-list-region"
              >
                {profiles.length === 0 ? (
                  <EmptyState
                    title={t(loaded ? "compatibility.empty" : "common.loading")}
                    description={t("compatibility.emptyHint")}
                  />
                ) : (
                  <div className="grid gap-2">
                    {profiles.map((profile) => (
                      <Panel
                        key={profile.id}
                        className="flex items-center gap-3 p-3"
                      >
                        <div className="min-w-0 flex-1">
                          <p className="truncate text-sm font-medium">
                            {label(profile)}
                          </p>
                          <p className="mt-1 text-xs text-muted-foreground">
                            {t(`compatibility.${profile.source}`)} ·{" "}
                            {t(
                              profile.confirmed_at
                                ? "compatibility.confirmed"
                                : "compatibility.candidate",
                            )}
                          </p>
                        </div>
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          disabled={busy}
                          aria-label={t("compatibility.reviewNamed", {
                            id: profile.id,
                          })}
                          onClick={() => {
                            if (serviceId)
                              void run(
                                () => getIdentityProfile(serviceId, profile.id),
                                received,
                              );
                          }}
                        >
                          {t("compatibility.review")}
                        </Button>
                      </Panel>
                    ))}
                  </div>
                )}
              </div>
            </>
          )}
        </DialogContent>
      </Dialog>
      <ConfirmDialog
        open={confirm !== null}
        title={t(
          confirm?.kind === "arm"
            ? "compatibility.arm"
            : confirm?.kind === "discard"
              ? "compatibility.discard"
              : "compatibility.confirm",
        )}
        description={t(
          confirm?.kind === "arm"
            ? "compatibility.armConsent"
            : confirm?.kind === "discard"
              ? "compatibility.discardConsent"
              : "compatibility.confirmConsent",
        )}
        confirmLabel={t(
          confirm?.kind === "arm"
            ? "compatibility.arm"
            : confirm?.kind === "discard"
              ? "compatibility.discard"
              : "compatibility.confirm",
        )}
        destructive={confirm?.kind === "discard"}
        disabled={busy}
        onCancel={() => setConfirm(null)}
        onConfirm={handleConfirm}
      />
    </div>
  );
}
