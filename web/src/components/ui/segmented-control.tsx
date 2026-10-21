import * as React from "react";
import { ToggleGroup as T } from "radix-ui";
import { cn } from "@/lib/utils";

/** One choice of a few, side by side: GitHub or GitLab, Day or Week. */
export function SegmentedControl({
  value,
  onValueChange,
  options,
  label,
  disabled,
  className,
}: {
  value: string;
  onValueChange: (v: string) => void;
  options: readonly { value: string; label: React.ReactNode }[];
  label: string;
  disabled?: boolean;
  className?: string;
}) {
  return (
    <T.Root
      type="single"
      value={value}
      // A segmented control always has one chosen: clicking it again keeps it.
      onValueChange={(v) => v && onValueChange(v)}
      aria-label={label}
      disabled={disabled}
      className={cn("inline-flex w-fit rounded-control border bg-muted p-0.5", className)}
    >
      {options.map((o) => (
        <T.Item
          key={o.value}
          value={o.value}
          className="h-10 cursor-pointer rounded-[calc(var(--radius-control)-2px)] px-3.5 text-body-sm font-medium text-muted-foreground disabled:cursor-not-allowed data-[state=on]:bg-surface data-[state=on]:text-foreground data-[state=on]:shadow-xs md:h-8"
        >
          {o.label}
        </T.Item>
      ))}
    </T.Root>
  );
}
