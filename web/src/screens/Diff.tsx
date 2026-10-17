import { Link, useParams } from "react-router";
import { APIError, useDiff, useItem } from "@/api/client";
import { Empty, ErrorBox, Loading } from "@/components/common";
import { DiffView } from "@/components/DiffView";

export function DiffScreen() {
  const id = Number(useParams().id);
  const item = useItem(id);
  const { data, error, isLoading } = useDiff(id);
  return (
    <div>
      <div className="mb-3 text-sm text-muted-foreground">
        <Link to="/" className="hover:text-foreground">
          Board
        </Link>{" "}
        /{" "}
        <Link to={`/items/${id}`} className="hover:text-foreground">
          #{item.data?.item.issue} {item.data?.item.title}
        </Link>{" "}
        / diff
      </div>
      {isLoading ? (
        <Loading what="Loading the diff" />
      ) : error instanceof APIError && error.status === 404 ? (
        <Empty>No reviewed diff for this item yet. It appears once a review has run.</Empty>
      ) : error ? (
        <ErrorBox error={error} />
      ) : (
        <>
          <p className="mb-3 text-xs text-muted-foreground">The patch exactly as review job #{data!.job_id} saw it, with its findings inline.</p>
          <DiffView patch={data!.patch} findings={data!.findings} itemId={id} />
        </>
      )}
    </div>
  );
}
