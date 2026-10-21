import { useParams } from "react-router";
import { APIError, useDiff, useItem } from "@/api/client";
import { Empty, ErrorBox, Loading, PageHeader } from "@/components/common";
import { DiffView } from "@/components/DiffView";

export function DiffScreen() {
  const id = Number(useParams().id);
  const item = useItem(id);
  const { data, error, isLoading } = useDiff(id);
  const it = item.data?.item;
  return (
    <div className="flex flex-col gap-5">
      <PageHeader
        crumbs={[
          { label: "The line", to: "/line" },
          { label: it ? `${it.ref} ${it.title}` : `Item ${id}`, to: `/items/${id}` },
        ]}
        title="The change"
        meta={data ? `The patch exactly as review job #${data.job_id} saw it, with its findings inline.` : undefined}
      />
      {isLoading ? (
        <Loading what="Loading the diff" />
      ) : error instanceof APIError && error.status === 404 ? (
        <Empty>No reviewed diff for this item yet. It appears once a review has run.</Empty>
      ) : error ? (
        <ErrorBox error={error} />
      ) : (
        <DiffView patch={data!.patch} findings={data!.findings} itemId={id} />
      )}
    </div>
  );
}
