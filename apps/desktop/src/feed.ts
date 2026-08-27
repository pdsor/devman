import { createContext, useContext } from "react";

import type { DaemonEvent } from "./api/types";

export interface Feed {
  events: DaemonEvent[];
  connected: boolean;
}

/** FeedContext shares one event stream with the whole window.
 *
 *  Pages derive their refresh signal from it: when the daemon says a service
 *  changed, the pages showing that service reload. Nothing polls for state that
 *  the daemon already announces. */
export const FeedContext = createContext<Feed>({ events: [], connected: false });

export function useFeed(): Feed {
  return useContext(FeedContext);
}

/** useFeedSignal returns a value that changes whenever a matching event arrives,
 *  suitable as the `signal` argument of useResource. */
export function useFeedSignal(match?: (event: DaemonEvent) => boolean): string {
  const { events } = useFeed();
  const relevant = match ? events.filter(match) : events;
  const last = relevant.at(-1);
  return last ? String(last.seq) : "0";
}
