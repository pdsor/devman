// Language selection.
//
// Chinese is the default because that is what this window is used in; the choice
// is remembered per machine and applied without a restart. Nothing here reaches
// the daemon: language is a property of the window, not of DevMan state.

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";

import { en, zh, type MessageKey } from "./messages";

export type Locale = "zh" | "en";

export type Translate = (key: MessageKey, vars?: Record<string, string | number>) => string;

interface LocaleState {
  locale: Locale;
  setLocale: (next: Locale) => void;
  t: Translate;
}

const STORAGE_KEY = "devman.locale";

const catalogues: Record<Locale, Record<MessageKey, string>> = { zh, en };

function initialLocale(): Locale {
  if (typeof localStorage !== "undefined") {
    const stored = localStorage.getItem(STORAGE_KEY);
    if (stored === "zh" || stored === "en") return stored;
  }
  // A first run follows the system, but only between the two languages that
  // exist; anything else lands on Chinese, which is the default.
  if (typeof navigator !== "undefined" && navigator.language.toLowerCase().startsWith("en")) {
    return "en";
  }
  return "zh";
}

const LocaleContext = createContext<LocaleState>({
  locale: "zh",
  setLocale: () => {},
  t: (key) => zh[key],
});

export function LocaleProvider(props: { children: ReactNode }) {
  const [locale, setStored] = useState<Locale>(initialLocale);

  const setLocale = useCallback((next: Locale) => {
    setStored(next);
    if (typeof localStorage !== "undefined") localStorage.setItem(STORAGE_KEY, next);
  }, []);

  useEffect(() => {
    document.documentElement.lang = locale === "zh" ? "zh-CN" : "en";
  }, [locale]);

  const value = useMemo<LocaleState>(() => {
    const catalogue = catalogues[locale];
    const t: Translate = (key, vars) => fill(catalogue[key], vars);
    return { locale, setLocale, t };
  }, [locale, setLocale]);

  return <LocaleContext.Provider value={value}>{props.children}</LocaleContext.Provider>;
}

export function useT(): Translate {
  return useContext(LocaleContext).t;
}

export function useLocale(): { locale: Locale; setLocale: (next: Locale) => void } {
  const { locale, setLocale } = useContext(LocaleContext);
  return { locale, setLocale };
}

/** fill substitutes {name} placeholders. A missing variable is left visible
 *  rather than silently blanked, so a bad call site shows up immediately. */
function fill(template: string, vars?: Record<string, string | number>): string {
  if (!vars) return template;
  return template.replace(/\{(\w+)\}/g, (match, name: string) =>
    name in vars ? String(vars[name]) : match,
  );
}

/**
 * describeFailure turns a coded failure from the Rust side into a sentence.
 *
 * The shell reports `DAEMON_NOT_RUNNING`, not a sentence, so the wording lives
 * here where the language is known. Anything unrecognised is shown verbatim —
 * hiding an unexpected failure behind "something went wrong" would be worse than
 * showing text in the wrong language.
 */
export function describeFailure(t: Translate, raw: string | null): string | null {
  if (!raw) return null;
  const [code = "", detail = ""] = raw.split(": ", 2);
  const key = `error.${code}` as MessageKey;
  if (!(key in zh)) return raw;
  const message = t(key);
  return detail ? `${message} ${detail}` : message;
}
