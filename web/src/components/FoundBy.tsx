import { Sparkles } from "lucide-react";
import { cn } from "@/lib/utils";

// Work the factory found and started on its own (trust level 6, main
// health, the orchestrator), told apart from what a person asked for.

export const sourceNames: Record<string, string> = {
  main_health: "main health",
  dependabot: "Dependabot",
  todos: "a TODO",
  sentry: "Sentry",
  hunt: "the bug hunt",
  orchestrator: "the orchestrator",
};

export const sourceName = (s: string) => sourceNames[s] ?? s.replace(/_/g, " ");

export function FoundBy({ source, className }: { source: string; className?: string }) {
  if (!source) return null;
  return (
    <span className={cn("inline-flex items-center gap-1 text-meta", className)} data-testid="found-by">
      <Sparkles className="size-3.5 shrink-0" aria-hidden />
      Found by the factory · {sourceName(source)}
    </span>
  );
}
