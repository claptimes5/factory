import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClientProvider } from "@tanstack/react-query";
import { createBrowserRouter, RouterProvider } from "react-router";
import { queryClient } from "@/api/client";
import { Layout } from "@/components/Layout";
import { BoardScreen } from "@/screens/Board";
import { ItemScreen } from "@/screens/Item";
import { SessionScreen } from "@/screens/Session";
import { DiffScreen } from "@/screens/Diff";
import { OrchestratorScreen } from "@/screens/Orchestrator";
import { MetricsScreen } from "@/screens/Metrics";
import { NotificationsScreen } from "@/screens/Notifications";
import { NotFound } from "@/screens/NotFound";
import { LiveScreen } from "@/screens/Live";
import { SettingsScreen } from "@/screens/Settings";
import { SetupScreen } from "@/screens/Setup";
import { GetStartedScreen } from "@/screens/GetStarted";
import "./index.css";

const router = createBrowserRouter(
  [
    {
      element: <Layout />,
      children: [
        { path: "/", element: <BoardScreen /> },
        { path: "/items/:id", element: <ItemScreen /> },
        { path: "/items/:id/diff", element: <DiffScreen /> },
        { path: "/jobs/:id", element: <SessionScreen /> },
        { path: "/orchestrator", element: <OrchestratorScreen /> },
        { path: "/metrics", element: <MetricsScreen /> },
        { path: "/notifications", element: <NotificationsScreen /> },
        { path: "/live", element: <LiveScreen /> },
        { path: "/settings", element: <SettingsScreen /> },
        { path: "/setup", element: <SetupScreen /> },
        { path: "/get-started", element: <GetStartedScreen /> },
        { path: "*", element: <NotFound /> },
      ],
    },
  ],
  { basename: "/app" },
);

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
);
