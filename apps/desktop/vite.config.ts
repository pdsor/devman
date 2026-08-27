import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The dev server port is fixed because Tauri's dev window loads it by URL; a
// port that moves would leave the window pointing at nothing.
const DEV_PORT = 5273;

export default defineConfig({
  plugins: [react()],
  // Tauri prints its own diagnostics into the same terminal.
  clearScreen: false,
  server: {
    port: DEV_PORT,
    strictPort: true,
    host: "127.0.0.1",
  },
  build: {
    // WebView2 on Windows and WebKit on macOS both handle modern output; there
    // is no legacy browser to support in a desktop shell.
    target: "esnext",
    sourcemap: true,
  },
});
