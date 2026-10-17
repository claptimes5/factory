import { Link } from "react-router-dom";

export function NotFound() {
  return (
    <div className="p-6 text-sm">
      Nothing here. <Link to="/" className="text-primary underline">Back to the board</Link>
    </div>
  );
}
