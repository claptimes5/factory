import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { fileURLToPath } from "node:url";

// `npm run dev` proxies the API to a running daemon (or `go run ./internal/web/devserver`).
const api = process.env.FACTORY_URL ?? "http://127.0.0.1:8090";

export default defineConfig({
  base: "/app/",
  plugins: [react(), tailwindcss()],
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  build: {
    outDir: "../internal/web/app",
    emptyOutDir: true,
    chunkSizeWarningLimit: 1500,
  },
  server: { proxy: { "/api": { target: api, ws: true, changeOrigin: false } } },
});
