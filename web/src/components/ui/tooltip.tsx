import * as React from "react";
import { Tooltip as T } from "radix-ui";
import { cn } from "@/lib/utils";

export const TooltipProvider = T.Provider;

/** A short hint on hover or focus. Never the only way to learn something: phones have no hover. */
export function Tooltip({ content, children, className }: { content: React.ReactNode; children: React.ReactElement; className?: string }) {
  return (
    <T.Root>
      <T.Trigger asChild>{children}</T.Trigger>
      <T.Portal>
        <T.Content sideOffset={6} className={cn("z-50 max-w-xs rounded-control bg-ink px-2.5 py-1.5 text-meta text-ink-foreground", className)}>
          {content}
        </T.Content>
      </T.Portal>
    </T.Root>
  );
}
