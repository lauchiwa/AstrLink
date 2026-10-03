import { useId, useState } from "react";
import { useT } from "@/i18n";
import type { CompatibilityDraft } from "@/request-compatibility-model";
import { Panel, PanelHeader } from "./Panel";
import { SegmentedControl } from "./SegmentedControl";
import { Button } from "./ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "./ui/popover";
import { Textarea } from "./ui/textarea";

/** Keep both JSON buffers in the parent draft so tab switches never discard edits. */
export function RequestRulesEditor({
  value,
  onChange,
}: {
  value: CompatibilityDraft;
  onChange: (value: CompatibilityDraft) => void;
}) {
  const t = useT();
  const id = useId();
  const [field, setField] = useState<"rules" | "headers">("rules");
  return (
    <Panel
      className="flex min-h-0 flex-1 flex-col"
      data-testid="request-rules-region"
    >
      <PanelHeader
        size="sm"
        actions={
          <Popover>
            <PopoverTrigger asChild>
              <Button type="button" variant="ghost" size="sm">
                {t("compatibility.help")}
              </Button>
            </PopoverTrigger>
            <PopoverContent className="w-80 max-h-80 overflow-y-auto text-sm">
              <p>{t("compatibility.rulesHelp")}</p>
              <p className="mt-2">{t("compatibility.headersHelp")}</p>
              <p className="mt-2">{t("compatibility.safetyHelp")}</p>
              <pre className="mt-3 overflow-x-auto font-mono text-xs">
                {JSON.stringify(
                  [
                    {
                      match: "model-id",
                      headers: { originator: "codex_exec" },
                      body: {},
                      enabled: true,
                    },
                  ],
                  null,
                  2,
                )}
              </pre>
            </PopoverContent>
          </Popover>
        }
      >
        <SegmentedControl
          label={t("compatibility.editor")}
          options={[
            { value: "rules", label: t("compatibility.rules") },
            { value: "headers", label: t("compatibility.headers") },
          ]}
          value={field}
          onValueChange={setField}
        />
      </PanelHeader>
      <label className="sr-only" htmlFor={id}>
        {t(
          field === "rules"
            ? "compatibility.rulesJSON"
            : "compatibility.headersJSON",
        )}
      </label>
      <Textarea
        id={id}
        aria-label={t(
          field === "rules"
            ? "compatibility.rulesJSON"
            : "compatibility.headersJSON",
        )}
        className="field-sizing-fixed min-h-0 flex-1 resize-none rounded-none border-0 p-3 font-mono text-xs shadow-none focus-visible:ring-inset"
        autoComplete="off"
        autoCorrect="off"
        spellCheck={false}
        value={value[field]}
        maxLength={262144}
        onChange={(event) =>
          onChange({ ...value, [field]: event.target.value })
        }
      />
    </Panel>
  );
}
