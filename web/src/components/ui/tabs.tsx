import * as React from "react";
import { Tabs as T } from "radix-ui";
import { cn } from "@/lib/utils";

export const Tabs = T.Root;

export const TabsList = React.forwardRef<React.ComponentRef<typeof T.List>, React.ComponentPropsWithoutRef<typeof T.List>>(({ className, ...props }, ref) => (
  <T.List ref={ref} className={cn("flex gap-1 overflow-x-auto border-b", className)} {...props} />
));
TabsList.displayName = "TabsList";

export const TabsTrigger = React.forwardRef<React.ComponentRef<typeof T.Trigger>, React.ComponentPropsWithoutRef<typeof T.Trigger>>(({ className, ...props }, ref) => (
  <T.Trigger
    ref={ref}
    className={cn(
      "-mb-px h-11 shrink-0 cursor-pointer border-b-2 border-transparent px-3 text-body-sm font-medium text-muted-foreground hover:text-foreground data-[state=active]:border-ink data-[state=active]:text-foreground md:h-9",
      className,
    )}
    {...props}
  />
));
TabsTrigger.displayName = "TabsTrigger";

export const TabsContent = React.forwardRef<React.ComponentRef<typeof T.Content>, React.ComponentPropsWithoutRef<typeof T.Content>>(({ className, ...props }, ref) => (
  <T.Content ref={ref} className={cn("pt-4 focus-visible:outline-none", className)} {...props} />
));
TabsContent.displayName = "TabsContent";
