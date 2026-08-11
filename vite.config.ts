import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  root: "frontend",
  publicDir: "public",
  server: {
    host: "127.0.0.1",
    port: 5173,
    // A changed port is a changed browser origin and therefore a different
    // IndexedDB database. Fail clearly instead of silently moving to 5174.
    strictPort: true,
    headers: { "Cache-Control": "no-store" },
  },
  preview: {
    host: "127.0.0.1",
    port: 4173,
    strictPort: true,
  },
  build: {
    outDir: "../internal/webui/assets",
    emptyOutDir: true,
    assetsDir: "dist",
    sourcemap: false,
  },
});
