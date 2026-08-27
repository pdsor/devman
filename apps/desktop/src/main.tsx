import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { App } from "./App";
import { LocaleProvider } from "./i18n";
import "./styles.css";

const host = document.getElementById("root");
if (!host) throw new Error("the DevMan window has no root element");

createRoot(host).render(
  <StrictMode>
    <LocaleProvider>
      <App />
    </LocaleProvider>
  </StrictMode>,
);
