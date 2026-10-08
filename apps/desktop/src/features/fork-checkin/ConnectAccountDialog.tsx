import { useEffect, useRef, useState } from "react";

import { ExternalLink } from "@/components/ExternalLink";
import { Field } from "@/components/Field";
import { FormMessage } from "@/components/FormMessage";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Textarea } from "@/components/ui/textarea";

import {
  beginCheckinAuthorization,
  CheckinBridgeError,
  completeCheckinAuthorization,
  describeCheckinError,
  getCheckinAccount,
  type CheckinResume,
} from "./bridge";
import { hostOf } from "./CheckinLists";
import { useCheckinT } from "./i18n";
import type { CheckinAccount, CheckinAuthorization } from "./model";
import { useAccountRequest } from "./use-account-request";

/**
 * Connects an account with a session the person captured in their own
 * browser.
 *
 * The site is an arbitrary third-party relay panel, so no vendor OAuth
 * applies and an in-app login window is not admitted on this platform yet.
 * The paste is handed to the host unchanged and converted there: this module
 * never parses, validates, stores or logs it, and it is cleared from state as
 * soon as it is handed over.
 */
export function ConnectAccountDialog({
  account,
  onClose,
  onConnected,
}: {
  account: CheckinAccount;
  onClose: () => void;
  onConnected: (account: CheckinAccount) => void;
}) {
  const t = useCheckinT();
  const [base, setBase] = useState(account);
  const [session, setSession] = useState<CheckinAuthorization | null>(null);
  const [pasted, setPasted] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [retryable, setRetryable] = useState(false);
  const [needsRestart, setNeedsRestart] = useState(false);
  const resume = useRef<CheckinResume | null>(null);
  const request = useAccountRequest();
  const host = hostOf(base.dashboard_base_url);

  // Codes that make the current session unusable: resubmitting against it
  // could only fail the same way, so the dialog asks for a fresh one.
  const restartCodes = [
    "revision_conflict",
    "authorization_not_found",
    "authorization_completed",
  ];
  const failed = (reason: unknown) => {
    resume.current =
      reason instanceof CheckinBridgeError ? reason.resume : null;
    const rejected = reason instanceof CheckinBridgeError ? reason : null;
    setRetryable(rejected?.retryable ?? false);
    if (rejected?.code && restartCodes.includes(rejected.code)) {
      setNeedsRestart(true);
      setSession(null);
    }
    setError(describeCheckinError(reason));
  };

  // One begin per session. Its session_id and the request_id Core issues are
  // reused by every submit below, so a retry replays Core's receipt instead
  // of acting twice.
  useEffect(() => {
    void request.run(
      () => beginCheckinAuthorization(base.id, base.revision),
      (started) => {
        setSession(started);
        setError(null);
      },
      failed,
    );
  }, []);

  // Fetched and begun as one action: `useAccountRequest` serializes the
  // dialog's commands, so a second call nested in this one's success would be
  // dropped by its own pending guard.
  const restart = () => {
    void request.run(
      async () => {
        const latest = await getCheckinAccount(base.id);
        return {
          latest,
          started: await beginCheckinAuthorization(latest.id, latest.revision),
        };
      },
      ({ latest, started }) => {
        resume.current = null;
        setBase(latest);
        setSession(started);
        setNeedsRestart(false);
        setRetryable(false);
        setError(null);
      },
      failed,
    );
  };

  const submit = () => {
    if (!session || !pasted.trim() || needsRestart) return;
    const handed = pasted;
    // Cleared before the await resolves: the dialog holds no copy while the
    // host is converting it, and none afterwards on either outcome.
    setPasted("");
    setError(null);
    void request.run(
      () =>
        completeCheckinAuthorization(
          {
            session_id: session.session_id,
            pasted_cookies: handed,
            dashboard_base_url: base.dashboard_base_url,
            expected_revision: base.revision,
          },
          resume.current,
        ),
      onConnected,
      failed,
    );
  };

  const ready = session !== null && !needsRestart;
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !request.isPending()) onClose();
      }}
    >
      <DialogContent
        className="sm:max-w-xl"
        onEscapeKeyDown={(event) => {
          if (request.isPending()) event.preventDefault();
        }}
        onInteractOutside={(event) => event.preventDefault()}
        showCloseButton={!request.busy}
        variant="workspace"
      >
        <DialogHeader className="shrink-0 border-b p-4 pr-10">
          <DialogTitle>{t("connect.title", { host })}</DialogTitle>
          <DialogDescription>{t("connect.description")}</DialogDescription>
        </DialogHeader>
        <div
          className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4"
          data-testid="checkin-connect-body"
        >
          <ol className="flex list-decimal flex-col gap-2 pl-5 text-sm text-text-secondary">
            <li>
              {t("connect.stepSignIn")}{" "}
              {session ? (
                <ExternalLink href={session.login_url}>
                  {t("connect.openSite", { host })}
                </ExternalLink>
              ) : null}
            </li>
            <li>{t("connect.stepCopy")}</li>
            <li>{t("connect.stepPaste")}</li>
          </ol>
          <Field
            className="min-h-0 flex-1"
            htmlFor="checkin-connect-paste"
            label={t("connect.pasteLabel")}
            hint={t("connect.pasteHint")}
          >
            <Textarea
              autoComplete="off"
              className="field-sizing-fixed min-h-32 flex-1 resize-none font-mono"
              disabled={!ready || request.busy}
              id="checkin-connect-paste"
              onChange={(event) => setPasted(event.target.value)}
              placeholder={t("connect.pastePlaceholder")}
              spellCheck={false}
              value={pasted}
            />
          </Field>
          <p className="text-xs text-muted-foreground">
            {t("connect.privacyNote")}
          </p>
          {error ? (
            <FormMessage tone="error">
              {error}
              {retryable ? ` ${t("connect.retryHint")}` : ""}
            </FormMessage>
          ) : null}
          {needsRestart ? (
            <Button variant="outline" disabled={request.busy} onClick={restart}>
              {t("dialog.reload")}
            </Button>
          ) : null}
        </div>
        <DialogFooter className="shrink-0 border-t p-4">
          <Button
            disabled={request.busy}
            onClick={() => {
              if (!request.isPending()) onClose();
            }}
            variant="outline"
          >
            {t("connect.cancel")}
          </Button>
          <Button
            disabled={!ready || request.busy || !pasted.trim()}
            onClick={submit}
          >
            {t(request.busy ? "connect.connecting" : "connect.confirm")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
