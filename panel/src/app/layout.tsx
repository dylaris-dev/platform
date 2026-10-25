// ci: trigger full pipeline run (no-op, safe to remove)
import type { Metadata } from "next";
import "./globals.css";
import { CONTRAST_BOOTSTRAP } from "@/lib/contrastMode";

// Self-hosted (src/app/fonts, see its README): next/font/google fetched the
// files at build time and a failed fetch broke the image build.
import localFont from "next/font/local";

const syne = localFont({
  src: "./fonts/Syne-Variable.woff2",
  weight: "700 800",
  variable: "--font-display",
});

const instrumentSans = localFont({
  src: "./fonts/InstrumentSans-Variable.woff2",
  weight: "400 600",
  variable: "--font-body",
});

const dmMono = localFont({
  src: [
    { path: "./fonts/DMMono-300.woff2", weight: "300", style: "normal" },
    { path: "./fonts/DMMono-400.woff2", weight: "400", style: "normal" },
    { path: "./fonts/DMMono-500.woff2", weight: "500", style: "normal" },
  ],
  variable: "--font-mono",
});

const vt323 = localFont({
  src: "./fonts/VT323-400.woff2",
  weight: "400",
  variable: "--font-logo",
});

export const metadata: Metadata = {
  title: "Dylaris",
  description: "Dylaris MC Webinterface",
};

// No force-dynamic, and no proxy.ts either: the panel is a STATIC EXPORT served
// by Core, so there is no Next server left to render per request.
//
// The nonce-strict CSP those two used to carry is not gone, it moved. The
// exported HTML is post-processed to carry a nonce PLACEHOLDER on every script
// tag (scripts/stamp-nonce.mjs), and Core swaps a fresh value in on every
// response along with the matching Content-Security-Policy header. Same policy,
// same per-response nonce, one fewer process.
//
// It has to stay a nonce rather than becoming a set of hashes: the Beam desktop
// client reads the nonce back out of the panel's CSP header and builds its own
// nonce-strict policy from it, and falls back to 'unsafe-inline' when it finds
// none. Hashes would downgrade the desktop app silently.

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html lang="en" className={`${syne.variable} ${instrumentSans.variable} ${dmMono.variable} ${vt323.variable}`} suppressHydrationWarning>
      {/* No runtime-config script here on purpose.

          Anything in the React tree is ALSO serialised into Next's flight
          payload, so a placeholder declared here appears twice in the HTML -
          once as a tag and once inside a JSON string. Replacing both would put
          unescaped quotes into that string and break hydration.

          Core injects the tag directly after <head> instead, where it is
          parser-inserted, nonced like every other script, and runs before the
          app bundle resolves its API URL. See core/panelfs (injectConfig). */}
      {/* Sets data-contrast before first paint so high contrast does not flash
          in after hydration; suppressHydrationWarning above is for exactly that
          attribute. It is a plain script tag, so stamp-nonce.mjs nonces it like
          the rest. */}
      <head>
        <script dangerouslySetInnerHTML={{ __html: CONTRAST_BOOTSTRAP }} />
      </head>
      <body>{children}</body>
    </html>
  );
}
