import * as React from "react";
import { Checkbox as C } from "radix-ui";
import { Check } from "lucide-react";
import { cn } from "@/lib/utils";

export const Checkbox = React.forwardRef<React.ComponentRef<typeof C.Root>, React.ComponentPropsWithoutRef<typeof C.Root>>(({ className, ...props }, ref) => (
  <C.Root
    ref={ref}
    className={cn(
      "peer size-5 shrink-0 cursor-pointer rounded-md border border-muted-foreground/60 bg-surface disabled:cursor-not-allowed disabled:opacity-50 data-[state=checked]:border-ink data-[state=checked]:bg-ink data-[state=checked]:text-ink-foreground",
      className,
    )}
    {...props}
  >
    <C.Indicator className="flex items-center justify-center">
      <Check className="size-3.5" strokeWidth={3} aria-hidden />
    </C.Indicator>
  </C.Root>
));
Checkbox.displayName = "Checkbox";
