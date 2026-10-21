import * as React from "react";
import { Accordion as A } from "radix-ui";
import { ChevronDown } from "lucide-react";
import { cn } from "@/lib/utils";

export const Accordion = A.Root;

export const AccordionItem = React.forwardRef<React.ComponentRef<typeof A.Item>, React.ComponentPropsWithoutRef<typeof A.Item>>(({ className, ...props }, ref) => (
  <A.Item ref={ref} className={cn("border-b last:border-b-0", className)} {...props} />
));
AccordionItem.displayName = "AccordionItem";

export const AccordionTrigger = React.forwardRef<React.ComponentRef<typeof A.Trigger>, React.ComponentPropsWithoutRef<typeof A.Trigger>>(
  ({ className, children, ...props }, ref) => (
    <A.Header className="flex">
      <A.Trigger
        ref={ref}
        className={cn(
          "flex min-h-11 flex-1 cursor-pointer items-center justify-between gap-3 py-3 text-left text-body font-medium md:min-h-9 [&[data-state=open]>svg]:rotate-180",
          className,
        )}
        {...props}
      >
        {children}
        <ChevronDown className="size-4 shrink-0 text-muted-foreground transition-transform" aria-hidden />
      </A.Trigger>
    </A.Header>
  ),
);
AccordionTrigger.displayName = "AccordionTrigger";

export const AccordionContent = React.forwardRef<React.ComponentRef<typeof A.Content>, React.ComponentPropsWithoutRef<typeof A.Content>>(
  ({ className, children, ...props }, ref) => (
    <A.Content ref={ref} className="overflow-hidden" {...props}>
      <div className={cn("pb-4 text-body-sm text-text-2", className)}>{children}</div>
    </A.Content>
  ),
);
AccordionContent.displayName = "AccordionContent";
