import { useRef, useState } from "react";

import { ConfirmDialog } from "@/components/ConfirmDialog";
import { FormMessage } from "@/components/FormMessage";
import { Button } from "@/components/ui/button";

import {
  CheckinBridgeError,
  deleteCheckinAccount,
  describeCheckinError,
  getCheckinAccount,
  type CheckinResume,
} from "./bridge";
import { hostOf } from "./CheckinLists";
import { useCheckinT } from "./i18n";
import type { CheckinAccount } from "./model";
import { useAccountRequest } from "./use-account-request";

export function DeleteAccountDialog({
  account,
  onClose,
  onDeleted,
}: {
  account: CheckinAccount;
  onClose: () => void;
  onDeleted: (id: string) => void;
}) {
  const t = useCheckinT();
  const [base, setBase] = useState(account);
  const [error, setError] = useState<string | null>(null);
  const [conflict, setConflict] = useState(false);
  const resume = useRef<CheckinResume | null>(null);
  const request = useAccountRequest();
  const failed = (reason: unknown, keepConflict = false) => {
    if (reason instanceof CheckinBridgeError && reason.code === "not_found") {
      onDeleted(base.id);
      return;
    }
    resume.current =
      reason instanceof CheckinBridgeError ? reason.resume : null;
    setConflict(
      (previous) =>
        (keepConflict && previous) ||
        (reason instanceof CheckinBridgeError &&
          reason.code === "revision_conflict"),
    );
    setError(describeCheckinError(reason));
  };
  const reload = () => {
    void request.run(
      () => getCheckinAccount(base.id),
      (latest) => {
        setBase(latest);
        resume.current = null;
        setConflict(false);
        setError(null);
      },
      (error) => failed(error, true),
    );
  };
  const remove = () => {
    if (conflict) return;
    void request.run(
      () => deleteCheckinAccount(base.id, base.revision, resume.current),
      () => onDeleted(base.id),
      failed,
    );
  };
  return (
    <ConfirmDialog
      open
      destructive
      disabled={request.busy}
      confirmDisabled={conflict}
      title={t("delete.title")}
      confirmLabel={t(request.busy ? "delete.deleting" : "delete.confirm")}
      cancelLabel={t("delete.cancel")}
      onCancel={() => {
        if (!request.isPending()) onClose();
      }}
      onConfirm={remove}
      description={
        <>
          <p>{t("delete.body", { host: hostOf(base.dashboard_base_url) })}</p>
          <p>{t("accounts.revision", { revision: base.revision })}</p>
          {error ? <FormMessage tone="error">{error}</FormMessage> : null}
          {conflict ? (
            <Button variant="outline" disabled={request.busy} onClick={reload}>
              {t("dialog.reload")}
            </Button>
          ) : null}
        </>
      }
    />
  );
}
