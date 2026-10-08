import { useLayoutEffect, useRef, useState } from "react";

/** Serializes a dialog's commands and drops callbacks after it is retired. */
export function useAccountRequest() {
  const generation = useRef(0);
  const pending = useRef(false);
  const active = useRef(false);
  const [busy, setBusy] = useState(false);
  useLayoutEffect(() => {
    active.current = true;
    generation.current += 1;
    return () => {
      active.current = false;
      generation.current += 1;
    };
  }, []);

  async function run<T>(
    action: () => Promise<T>,
    success: (value: T) => void,
    failure: (error: unknown) => void,
  ) {
    if (!active.current || pending.current) return;
    pending.current = true;
    setBusy(true);
    const current = generation.current;
    try {
      const value = await action();
      if (generation.current === current) success(value);
    } catch (error) {
      if (generation.current === current) failure(error);
    } finally {
      if (generation.current === current) {
        pending.current = false;
        setBusy(false);
      }
    }
  }
  return { busy, run, isPending: () => pending.current };
}
