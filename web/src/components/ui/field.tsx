import * as React from "react";
import { cn } from "@/lib/utils";

/**
 * A form field: its label, the control, then help or the error. Give it
 * the control's id; it ties the label, help and error to it.
 */
export function Field({
  id,
  label,
  help,
  error,
  className,
  children,
}: {
  id: string;
  label: React.ReactNode;
  help?: React.ReactNode;
  error?: React.ReactNode;
  className?: string;
  children: React.ReactElement<{ "aria-describedby"?: string; "aria-invalid"?: boolean }>;
}) {
  const describedBy = error ? `${id}-error` : help ? `${id}-help` : undefined;
  return (
    <div className={cn("flex min-w-0 flex-col gap-1.5", className)}>
      <label htmlFor={id} className="text-body-sm font-medium">
        {label}
      </label>
      {React.cloneElement(children, { "aria-describedby": describedBy, "aria-invalid": error ? true : undefined })}
      {error ? (
        <span id={`${id}-error`} className="text-meta text-destructive">
          {error}
        </span>
      ) : (
        help && (
          <span id={`${id}-help`} className="text-meta text-muted-foreground">
            {help}
          </span>
        )
      )}
    </div>
  );
}
