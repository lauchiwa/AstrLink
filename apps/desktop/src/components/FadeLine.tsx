import { useLayoutEffect, useRef, useState, type CSSProperties } from "react";

import { cn } from "@/lib/utils";

const fadeEnd: CSSProperties = {
  maskImage: "linear-gradient(to right, #000 calc(100% - 6em), transparent)",
};

/**
 * One line of text. Text that runs past the width fades out at the end
 * instead of ending on an ellipsis; text that fits is left as it is.
 */
export function FadeLine({
  text,
  className,
}: {
  text: string;
  className?: string;
}) {
  const ref = useRef<HTMLSpanElement>(null);
  const [clipped, setClipped] = useState(false);
  useLayoutEffect(() => {
    const element = ref.current;
    if (!element) return;
    const measure = () =>
      setClipped(element.scrollWidth > element.clientWidth + 1);
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(measure);
    observer.observe(element);
    return () => observer.disconnect();
  }, [text]);
  return (
    <span
      className={cn("block overflow-hidden whitespace-nowrap", className)}
      data-clipped={clipped || undefined}
      data-slot="fade-line"
      ref={ref}
      style={clipped ? fadeEnd : undefined}
    >
      {text}
    </span>
  );
}
