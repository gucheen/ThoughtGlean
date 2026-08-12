import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  test: {
    environment: "jsdom",
    setupFiles: ["./frontend/src/test/setup.ts"],
    include: ["frontend/src/**/*.test.{ts,tsx}"],
    clearMocks: true,
    fileParallelism: false,
  },
});
