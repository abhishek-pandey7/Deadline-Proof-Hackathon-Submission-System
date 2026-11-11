import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// In development the Go API runs on :8080 and Vite proxies to it, so the
// browser sees one origin and the session cookie stays first-party, exactly
// as in production where the Go binary serves this build itself.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    port: 5173,
    proxy: {
      "/api": { target: "http://127.0.0.1:8080", changeOrigin: false },
      "/healthz": "http://127.0.0.1:8080",
    },
  },
  build: {
    // The Go binary embeds this folder when built with -tags embedui.
    outDir: "../internal/webui/dist",
    emptyOutDir: true,
    sourcemap: false,
  },
});
