import * as React from "react";
import { Slot } from "@radix-ui/react-slot";
import { clsx } from "clsx";
import { twMerge } from "tailwind-merge";

// shadcn-style, locally owned component: Radix composition with native semantics.
export function Button({
  className,
  variant = "default",
  asChild = false,
  loading = false,
  children,
  disabled,
  type,
  ...props
}: React.ComponentProps<"button"> & {
  variant?: "default" | "outline" | "danger";
  asChild?: boolean;
  loading?: boolean;
}) {
  const Comp = asChild ? Slot : "button";
  return (
    <Comp
      type={asChild ? undefined : (type ?? "button")}
      disabled={asChild ? undefined : disabled || loading}
      aria-busy={loading || undefined}
      data-loading={loading || undefined}
      className={twMerge(clsx("button", `button-${variant}`, className))}
      {...props}
    >
      {asChild ? children : <>{loading && <span className="button-loader" aria-hidden="true">/</span>}{children}</>}
    </Comp>
  );
}
