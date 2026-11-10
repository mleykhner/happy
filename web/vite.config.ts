import { defineConfig } from "vite";
import tailwindcss from "@tailwindcss/vite";
export default defineConfig({
  plugins: [tailwindcss()],
  build: { outDir: "../internal/webui/dist", emptyOutDir: true },
  server: { proxy: { "/api": "http://127.0.0.1:8080" } },
});
