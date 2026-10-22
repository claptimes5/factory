import * as React from "react";
import { Slot } from "radix-ui";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

// Buttons are 44px tall on a phone, so a thumb finds them, and tighter
// on a desk. The main action is ink (dark on light, light on dark);
// inverse sits on the lead card (see index.css), where it follows the
// card's own ink colors.
const buttonVariants = cva(
  "inline-flex items-center justify-center gap-1.5 whitespace-nowrap rounded-control font-medium transition-colors disabled:pointer-events-none disabled:opacity-50 [&_svg]:size-4 [&_svg]:shrink-0 cursor-pointer",
  {
    variants: {
      variant: {
        default: "bg-ink text-ink-foreground hover:bg-ink/90",
        primary: "bg-primary text-primary-foreground hover:bg-primary/90",
        destructive: "bg-destructive text-white hover:bg-destructive/90",
        outline: "border bg-surface hover:bg-accent",
        ghost: "hover:bg-accent",
        link: "text-primary underline-offset-4 hover:underline",
        inverse: "bg-ink-foreground text-ink hover:bg-ink-foreground/90",
        "inverse-outline": "border border-ink-border text-ink-foreground hover:bg-ink-foreground/10",
      },
      size: {
        default: "h-11 px-4 text-body md:h-9 md:px-3 md:text-body-sm",
        sm: "h-10 px-3 text-body-sm md:h-8 md:px-2.5 md:text-meta",
        lg: "h-12 px-5 text-body",
        icon: "size-11 md:size-9",
      },
    },
    defaultVariants: { variant: "default", size: "default" },
  },
);

export interface ButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement>, VariantProps<typeof buttonVariants> {
  /** Render the child (a link, say) with the button's look. */
  asChild?: boolean;
}

export const Button = React.forwardRef<HTMLButtonElement, ButtonProps>(({ className, variant, size, asChild, ...props }, ref) => {
  const Comp = asChild ? Slot.Root : "button";
  return <Comp ref={ref} className={cn(buttonVariants({ variant, size }), className)} {...props} />;
});
Button.displayName = "Button";
