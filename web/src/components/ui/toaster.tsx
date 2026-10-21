import { Toaster as Sonner } from "sonner";

export { toast } from "sonner";

/** Where toasts show: bottom centre, above a phone's tab bar. */
export function Toaster() {
  return (
    <Sonner
      position="bottom-center"
      offset={{ bottom: "calc(5rem + env(safe-area-inset-bottom))" }}
      toastOptions={{
        classNames: {
          toast: "rounded-card! border! bg-surface! text-foreground! font-sans! text-body-sm!",
          description: "text-muted-foreground!",
        },
      }}
    />
  );
}
