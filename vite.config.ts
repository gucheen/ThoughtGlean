import { defineConfig, loadEnv } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, ".", "THOUGHTGLEAN_");
  const buildID = Date.now().toString(36);
  return ({
  plugins: [
    react(),
    {
      name: "thoughtglean-build-version",
      transformIndexHtml(html) {
        return html.replace("</head>", `    <meta name="thoughtglean-build" content="${buildID}" />\n  </head>`);
      },
    },
  ],
  root: "frontend",
  publicDir: "public",
  server: {
    host: "127.0.0.1",
    port: 5173,
    // A changed port is a changed browser origin and therefore a different
    // IndexedDB database. Fail clearly instead of silently moving to 5174.
    strictPort: true,
    headers: { "Cache-Control": "no-store" },
    proxy: {
      "/api": { target: env.THOUGHTGLEAN_DEV_API_URL || "http://127.0.0.1:8080" },
    },
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
});
