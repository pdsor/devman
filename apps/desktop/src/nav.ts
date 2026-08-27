import { createContext, useContext } from "react";

/** Route is the window's location. There is no URL bar in a desktop shell, so
 *  routes are plain values rather than parsed strings. */
export type Route =
  | { page: "projects" }
  | { page: "project"; id: string }
  | { page: "logs"; id?: string; service?: string }
  | { page: "ports"; port?: number }
  | { page: "register" }
  | { page: "config"; id: string }
  | { page: "events" }
  | { page: "environment" }
  | { page: "settings" };

export const NavContext = createContext<(route: Route) => void>(() => {});

export function useNav(): (route: Route) => void {
  return useContext(NavContext);
}
