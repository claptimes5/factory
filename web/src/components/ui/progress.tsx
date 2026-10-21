import { Progress as P } from "radix-ui";
import { cn } from "@/lib/utils";

export function Progress({ value, max = 100, label, className, tone = "primary" }: { value: number; max?: number; label: string; className?: string; tone?: "primary" | "success" | "attention" }) {
  return (
    <P.Root value={value} max={max} aria-label={label} className={cn("relative h-1.5 w-full overflow-hidden rounded-full bg-border", className)}>
      <P.Indicator
        className={cn("h-full rounded-full transition-[width]", tone === "success" ? "bg-success-fill" : tone === "attention" ? "bg-warning-fill" : "bg-primary")}
        style={{ width: `${max > 0 ? Math.min(100, (value / max) * 100) : 0}%` }}
      />
    </P.Root>
  );
}
