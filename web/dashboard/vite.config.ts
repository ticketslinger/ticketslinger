import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// `vite` alone serves the UI with hot reload; /api and /session go to the dashboard server.
const server = process.env.TS_DASHBOARD_URL ?? "http://127.0.0.1:3002";

export default defineConfig({
  plugins: [react()],
  build: { outDir: "dist", emptyOutDir: true },
  server: { proxy: { "/api": server, "/session": server, "/config.json": server } },
});
