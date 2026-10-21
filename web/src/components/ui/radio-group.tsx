import * as React from "react";
import { RadioGroup as R } from "radix-ui";
import { cn } from "@/lib/utils";

export const RadioGroup = React.forwardRef<React.ComponentRef<typeof R.Root>, React.ComponentPropsWithoutRef<typeof R.Root>>(({ className, ...props }, ref) => (
  <R.Root ref={ref} className={cn("flex flex-col gap-2.5", className)} {...props} />
));
RadioGroup.displayName = "RadioGroup";

export const RadioGroupItem = React.forwardRef<React.ComponentRef<typeof R.Item>, React.ComponentPropsWithoutRef<typeof R.Item>>(({ className, ...props }, ref) => (
  <R.Item
    ref={ref}
    className={cn("size-5 shrink-0 cursor-pointer rounded-full border bg-surface disabled:cursor-not-allowed disabled:opacity-50 data-[state=checked]:border-ink", className)}
    {...props}
  >
    <R.Indicator className="flex items-center justify-center after:block after:size-2.5 after:rounded-full after:bg-ink" />
  </R.Item>
));
RadioGroupItem.displayName = "RadioGroupItem";
