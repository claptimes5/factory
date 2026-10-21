import * as React from "react";
import { Switch as S } from "radix-ui";
import { cn } from "@/lib/utils";

export const Switch = React.forwardRef<React.ComponentRef<typeof S.Root>, React.ComponentPropsWithoutRef<typeof S.Root>>(({ className, ...props }, ref) => (
  <S.Root
    ref={ref}
    className={cn(
      "peer inline-flex h-7 w-12 shrink-0 cursor-pointer items-center rounded-full border-2 border-transparent bg-muted transition-colors disabled:cursor-not-allowed disabled:opacity-50 data-[state=checked]:bg-primary",
      className,
    )}
    {...props}
  >
    <S.Thumb className="block size-6 rounded-full bg-white shadow-sm transition-transform data-[state=checked]:translate-x-5 data-[state=unchecked]:translate-x-0" />
  </S.Root>
));
Switch.displayName = "Switch";
