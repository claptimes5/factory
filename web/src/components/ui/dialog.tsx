import * as React from "react";
import { Dialog as D } from "radix-ui";
import { cva, type VariantProps } from "class-variance-authority";
import { X } from "lucide-react";
import { cn } from "@/lib/utils";

export const Dialog = D.Root;
export const DialogTrigger = D.Trigger;
export const DialogClose = D.Close;

const overlay = "fixed inset-0 z-50 bg-black/40";

// A dialog in the middle of a desk; a sheet comes up from the bottom of a
// phone (or in from the right on a desk), for anything longer.
const panel = cva("fixed z-50 flex flex-col gap-4 bg-surface p-5 text-foreground shadow-xl focus:outline-none", {
  variants: {
    side: {
      center: "top-1/2 left-1/2 w-[calc(100%-2rem)] max-w-lg -translate-x-1/2 -translate-y-1/2 rounded-card border",
      bottom: "inset-x-0 bottom-0 max-h-[90dvh] overflow-y-auto rounded-t-card border-t pb-[max(1.25rem,env(safe-area-inset-bottom))]",
      right: "inset-y-0 right-0 w-full max-w-md overflow-y-auto border-l",
    },
  },
  defaultVariants: { side: "center" },
});

export const DialogContent = React.forwardRef<
  React.ComponentRef<typeof D.Content>,
  React.ComponentPropsWithoutRef<typeof D.Content> & VariantProps<typeof panel>
>(({ className, children, side, ...props }, ref) => (
  <D.Portal>
    <D.Overlay className={overlay} />
    <D.Content ref={ref} className={cn(panel({ side }), className)} {...props}>
      {side === "bottom" && <div aria-hidden className="mx-auto -mt-2 h-1 w-10 rounded-full bg-border" />}
      {children}
      <D.Close className="absolute top-3 right-3 inline-flex size-11 cursor-pointer items-center justify-center rounded-control text-muted-foreground hover:bg-accent md:size-9">
        <X className="size-4" aria-hidden />
        <span className="sr-only">Close</span>
      </D.Close>
    </D.Content>
  </D.Portal>
));
DialogContent.displayName = "DialogContent";

export const DialogTitle = React.forwardRef<React.ComponentRef<typeof D.Title>, React.ComponentPropsWithoutRef<typeof D.Title>>(({ className, ...props }, ref) => (
  <D.Title ref={ref} className={cn("pr-10 text-title-sm font-semibold", className)} {...props} />
));
DialogTitle.displayName = "DialogTitle";

export const DialogDescription = React.forwardRef<React.ComponentRef<typeof D.Description>, React.ComponentPropsWithoutRef<typeof D.Description>>(
  ({ className, ...props }, ref) => <D.Description ref={ref} className={cn("text-body-sm text-text-2", className)} {...props} />,
);
DialogDescription.displayName = "DialogDescription";

/** A sheet: a dialog from the bottom of the screen. */
export const SheetContent = React.forwardRef<React.ComponentRef<typeof D.Content>, React.ComponentPropsWithoutRef<typeof D.Content> & { side?: "bottom" | "right" }>(
  ({ side = "bottom", ...props }, ref) => <DialogContent ref={ref} side={side} {...props} />,
);
SheetContent.displayName = "SheetContent";
