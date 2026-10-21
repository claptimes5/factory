import * as React from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

const alertVariants = cva("flex gap-3 rounded-card border p-3.5 text-body-sm [&>svg]:mt-0.5 [&>svg]:size-4 [&>svg]:shrink-0", {
  variants: {
    variant: {
      info: "border-live-border bg-live-bg text-foreground [&>svg]:text-primary",
      attention: "border-warning-border bg-warning-bg text-foreground [&>svg]:text-warning",
      success: "border-transparent bg-success-bg text-foreground [&>svg]:text-success",
      danger: "border-destructive/30 bg-destructive/5 text-foreground [&>svg]:text-destructive",
    },
  },
  defaultVariants: { variant: "info" },
});

export function Alert({ className, variant, ...props }: React.HTMLAttributes<HTMLDivElement> & VariantProps<typeof alertVariants>) {
  return <div role={variant === "danger" || variant === "attention" ? "alert" : "status"} className={cn(alertVariants({ variant }), className)} {...props} />;
}
