import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClientProvider } from "@tanstack/react-query";
import { createBrowserRouter, Navigate, RouterProvider } from "react-router";
import { queryClient } from "@/api/client";
import { Layout } from "@/components/Layout";
import { BoardScreen } from "@/screens/Board";
import { HomeScreen } from "@/screens/Home";
import { ItemScreen } from "@/screens/Item";
import { SessionScreen } from "@/screens/Session";
import { DiffScreen } from "@/screens/Diff";
import { OrchestratorScreen } from "@/screens/Orchestrator";
import { MetricsScreen } from "@/screens/Metrics";
import { NotificationsScreen } from "@/screens/Notifications";
import { NotFound } from "@/screens/NotFound";
import { HistoryScreen } from "@/screens/History";
import { TrustScreen } from "@/screens/Trust";
import { NotifySettingsScreen } from "@/screens/NotifySettings";
import { SettingsScreen } from "@/screens/Settings";
import { SetupScreen } from "@/screens/Setup";
import { GetStartedScreen } from "@/screens/GetStarted";
import { DesignScreen } from "@/screens/Design";
import { Toaster } from "@/components/ui/toaster";
import { TooltipProvider } from "@/components/ui/tooltip";
import "./index.css";

const router = createBrowserRouter(
  [
    {
      element: <Layout />,
      children: [
        { path: "/", element: <HomeScreen /> },
        { path: "/line", element: <BoardScreen /> },
        { path: "/items/:id", element: <ItemScreen /> },
        { path: "/items/:id/diff", element: <DiffScreen /> },
        { path: "/jobs/:id", element: <SessionScreen /> },
        { path: "/orchestrator", element: <OrchestratorScreen /> },
        { path: "/metrics", element: <MetricsScreen /> },
        { path: "/notifications", element: <NotificationsScreen /> },
        { path: "/history", element: <HistoryScreen /> },
        { path: "/trust", element: <TrustScreen /> },
        { path: "/notify", element: <NotifySettingsScreen /> },
        { path: "/live", element: <Navigate to="/history" replace /> },
        { path: "/settings", element: <SettingsScreen /> },
        { path: "/setup", element: <SetupScreen /> },
        { path: "/get-started", element: <GetStartedScreen /> },
        { path: "/design", element: <DesignScreen /> },
        { path: "*", element: <NotFound /> },
      ],
    },
  ],
  { basename: "/app" },
);

// The service worker shows push notifications; it caches nothing.
if ("serviceWorker" in navigator) {
  void navigator.serviceWorker.register("/app/sw.js", { scope: "/app/" }).catch(() => {
    /* without it, push is unavailable: the notifications screen says so */
  });
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <TooltipProvider delayDuration={300}>
        <RouterProvider router={router} />
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>
  </StrictMode>,
);
