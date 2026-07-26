import { defineConfig, loadEnv } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, ".", "");
  const backend = env.CF_R2_MANAGER_BACKEND || "http://127.0.0.1:8080";
  return {
    plugins: [react()],
    build: {
      target: "es2022",
      sourcemap: false,
      outDir: "dist",
      emptyOutDir: true,
    },
    server: {
      proxy: {
        "/api": backend,
        "/healthz": backend,
        "/readyz": backend,
      },
    },
  };
});
