/// <reference types="vite/client" />

// The UI-review switch. Typed here rather than read as a loose string so a
// misspelled variable name fails the type check instead of silently doing
// nothing: a review mode that quietly stays off is worse than no review mode.
interface ImportMetaEnv {
  /** Connect straight to the fixture daemon instead of asking for an address. */
  readonly VITE_DEVMAN_UI_TEST?: string;
  /** Where the fixture is listening. Defaults to http://127.0.0.1:39190/api/v1. */
  readonly VITE_DEVMAN_UI_TEST_URL?: string;
  /** The fixture's token. Defaults to the fixture's own fixed value. */
  readonly VITE_DEVMAN_UI_TEST_TOKEN?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
