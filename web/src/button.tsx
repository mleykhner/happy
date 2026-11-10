import * as React from "react";
import { Slot } from "@radix-ui/react-slot";
import { clsx } from "clsx";
import { twMerge } from "tailwind-merge";

// shadcn-style, locally owned component: Radix composition with native semantics.
export function Button({
  className,
  variant = "default",
  asChild = false,
  ...props
}: React.ComponentProps<"button"> & {
  variant?: "default" | "outline" | "danger";
  asChild?: boolean;
}) {
  const Comp = asChild ? Slot : "button";
  return (
    <Comp
      className={twMerge(clsx("button", `button-${variant}`, className))}
      {...props}
    />
  );
}
