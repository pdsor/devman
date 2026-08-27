import { createContext, useContext } from "react";

import type { Api } from "./client";

/** ApiContext carries the connected client. Pages are only mounted once a
 *  daemon has answered, so the value is non-null for every consumer. */
export const ApiContext = createContext<Api | null>(null);

export function useApi(): Api {
  const api = useContext(ApiContext);
  if (!api) throw new Error("useApi was called outside a connected DevMan window");
  return api;
}
