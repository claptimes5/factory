import { Link } from "react-router";
import { useOverview } from "@/api/client";
import { ActivityFeed, LiveHeading, WeekStats } from "@/components/Activity";
import { useRepo } from "@/lib/repo";

export function LiveScreen() {
  const repo = useRepo(useOverview().data?.repos ?? []);
  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-5">
      <div className="flex items-center justify-between">
        <LiveHeading>Live activity</LiveHeading>
        <Link to="/orchestrator" className="text-sm text-primary hover:underline">
          Orchestrator decisions
        </Link>
      </div>
      <WeekStats />
      <ActivityFeed repo={repo} />
    </div>
  );
}
