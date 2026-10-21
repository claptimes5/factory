import * as React from "react";
import { cn } from "@/lib/utils";

// 16px text on a phone, so iOS doesn't zoom into the field.
const field =
  "w-full min-w-0 rounded-control border bg-surface px-3 text-base text-foreground placeholder:text-muted-foreground/80 focus-visible:border-ring focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30 disabled:opacity-60 aria-invalid:border-destructive md:text-body-sm";

export const Input = React.forwardRef<HTMLInputElement, React.InputHTMLAttributes<HTMLInputElement>>(({ className, ...props }, ref) => (
  <input ref={ref} className={cn(field, "h-11 md:h-9", className)} {...props} />
));
Input.displayName = "Input";

export const Textarea = React.forwardRef<HTMLTextAreaElement, React.TextareaHTMLAttributes<HTMLTextAreaElement>>(({ className, ...props }, ref) => (
  <textarea ref={ref} className={cn(field, "min-h-24 py-2.5", className)} {...props} />
));
Textarea.displayName = "Textarea";

/** A select that opens the device's own picker, the easiest on a phone. */
export const NativeSelect = React.forwardRef<HTMLSelectElement, React.SelectHTMLAttributes<HTMLSelectElement>>(({ className, ...props }, ref) => (
  <select ref={ref} className={cn(field, "h-11 cursor-pointer md:h-9", className)} {...props} />
));
NativeSelect.displayName = "NativeSelect";
