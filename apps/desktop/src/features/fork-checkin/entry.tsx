import { Component, lazy, Suspense, type ReactNode } from "react";

import { EmptyState } from "@/components/EmptyState";
import { LoadingState } from "@/components/LoadingState";

import type { CheckinServiceCatalog } from "./AccountDialog";
import { checkinT, useCheckinT } from "./i18n";

// The workspace and its bridge load only when the page is opened, so the
// shared shell neither pays for nor calls into the extension until then.
const CheckinWorkspace = lazy(() =>
  import("./Workspace").then((module) => ({
    default: module.CheckinWorkspace,
  })),
);

/** The shared navigation's label; the shell re-renders on locale change. */
export function checkinNavLabel(): string {
  return checkinT("nav.label");
}

class CheckinBoundary extends Component<
  { children: ReactNode },
  { failed: boolean }
> {
  state = { failed: false };

  static getDerivedStateFromError() {
    return { failed: true };
  }

  render() {
    return this.state.failed ? <PageFailed /> : this.props.children;
  }
}

function PageFailed() {
  const t = useCheckinT();
  return (
    <EmptyState
      description={t("workspace.pageFailedHint")}
      title={t("workspace.pageFailed")}
    />
  );
}

function PageLoading() {
  const t = useCheckinT();
  return (
    <div className="flex min-h-0 flex-1 justify-center py-10">
      <LoadingState label={t("workspace.loading")} />
    </div>
  );
}

/**
 * The only thing the shared shell imports from this feature. A failure to
 * load or render stays inside this boundary instead of the app-wide one.
 */
export function CheckinWorkspaceEntry(
  props: CheckinServiceCatalog & {
    coreSessionKey: string | null;
    isReady: boolean;
  },
) {
  return (
    <CheckinBoundary>
      <Suspense fallback={<PageLoading />}>
        <CheckinWorkspace {...props} />
      </Suspense>
    </CheckinBoundary>
  );
}
