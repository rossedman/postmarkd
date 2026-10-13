// Builds postmarkd's brand assets into ../ (assets/brand):
//
//   mark.svg, logo.svg, favicon.svg   vector, text converted to outlines,
//                                     adapt to light/dark color schemes
//   banner-light.png, banner-dark.png README header (2x)
//   social-card.png                   1280x640 Open Graph / GitHub preview
//
//   cd assets/brand/src && npm install && npm run build
//
// Fonts are downloaded from Google Fonts on first run (or set FONT_DIR).
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import opentype from "opentype.js";

const here = path.dirname(fileURLToPath(import.meta.url));
const out = path.resolve(here, "..");
const fontDir = process.env.FONT_DIR || path.join(here, ".fonts");

export const palette = {
  paper: "#FBF8F2", ink: "#16171D", muted: "#5D606B", stamp: "#E0492A",
  night: "#0F1015", chalk: "#EEEAE2", stampDark: "#FF6B47",
};

const fonts = {
  "SpaceGrotesk-Bold": "https://fonts.googleapis.com/css2?family=Space+Grotesk:wght@700",
  "JetBrainsMono-SemiBold": "https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@600",
};

async function loadFont(name) {
  const file = path.join(fontDir, name + ".ttf");
  if (!fs.existsSync(file)) {
    fs.mkdirSync(fontDir, { recursive: true });
    const css = await (await fetch(fonts[name])).text(); // no UA: Google serves TTF
    const url = css.match(/https:[^)]+\.ttf/)[0];
    fs.writeFileSync(file, Buffer.from(await (await fetch(url)).arrayBuffer()));
  }
  return opentype.loadSync(file);
}

// The mark: a circular postmark with a "d" (for daemon) and three wavy
// cancellation lines. 96x96 units.
function markShapes(d) {
  const waves = [34, 48, 62].map(y => `M61 ${y} q4.25 -5.5 8.5 0 t8.5 0 t8.5 0 t8.5 0`).join(" ");
  return {
    ring: `<circle class="stamp" cx="30" cy="48" r="23" fill="none" stroke-width="6"/>` +
          `<circle class="stamp" cx="30" cy="48" r="15.5" fill="none" stroke-width="1.6" stroke-dasharray="2.2 2.6"/>`,
    d: `<path class="stamp-fill" d="${d}"/>`,
    waves: `<path class="ink" d="${waves}" fill="none" stroke-width="4.5" stroke-linecap="round"/>`,
  };
}

function style(adaptive) {
  const base = `.stamp{stroke:${palette.stamp}}.stamp-fill{fill:${palette.stamp}}` +
    `.ink{stroke:${palette.ink}}.ink-fill{fill:${palette.ink}}`;
  if (!adaptive) return `<style>${base}</style>`;
  return `<style>${base}@media (prefers-color-scheme: dark){` +
    `.stamp{stroke:${palette.stampDark}}.stamp-fill{fill:${palette.stampDark}}` +
    `.ink{stroke:${palette.chalk}}.ink-fill{fill:${palette.chalk}}}</style>`;
}

function glyphPath(font, text, x, baseline, size) {
  return font.getPath(text, x, baseline, size).toPathData(2);
}

async function buildSVGs() {
  const grotesk = await loadFont("SpaceGrotesk-Bold");

  // "d" centred in the ring.
  const dSize = 30;
  const dBox = grotesk.getPath("d", 0, 0, dSize).getBoundingBox();
  const dx = 30 - (dBox.x1 + dBox.x2) / 2, dy = 48 - (dBox.y1 + dBox.y2) / 2;
  const d = glyphPath(grotesk, "d", dx, dy, dSize);
  const m = markShapes(d);
  const mark = m.ring + m.d + m.waves;

  const svg = (w, h, body, adaptive = true, title = "postmarkd") =>
    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${w} ${h}" width="${w}" height="${h}" role="img" aria-label="${title}">` +
    `<title>${title}</title>${style(adaptive)}${body}</svg>\n`;

  fs.writeFileSync(path.join(out, "mark.svg"), svg(96, 96, mark));
  // Favicon: just the stamp, heavier, so it survives 16px.
  const fSize = 34;
  const fBox = grotesk.getPath("d", 0, 0, fSize).getBoundingBox();
  const fd = glyphPath(grotesk, "d", 30 - (fBox.x1 + fBox.x2) / 2, 48 - (fBox.y1 + fBox.y2) / 2, fSize);
  const favicon = `<circle class="stamp" cx="30" cy="48" r="23" fill="none" stroke-width="7"/><path class="stamp-fill" d="${fd}"/>`;
  fs.writeFileSync(path.join(out, "favicon.svg"), svg(96, 96, favicon).replace('viewBox="0 0 96 96"', 'viewBox="3 21 54 54"'));

  // Wordmark: "postmark" in ink, "d" in stamp red.
  const size = 56, baseline = 66, x0 = 108;
  const ink = glyphPath(grotesk, "postmark", x0, baseline, size);
  const inkAdv = grotesk.getAdvanceWidth("postmark", size) - 1.5;
  const red = glyphPath(grotesk, "d", x0 + inkAdv, baseline, size);
  const width = Math.ceil(x0 + inkAdv + grotesk.getAdvanceWidth("d", size) + 4);
  const word = `<path class="ink-fill" d="${ink}"/><path class="stamp-fill" d="${red}"/>`;
  fs.writeFileSync(path.join(out, "logo.svg"), svg(width, 96, mark + word));
  // Fixed-color variants for places that ignore prefers-color-scheme.
  fs.writeFileSync(path.join(out, "logo-light.svg"), svg(width, 96, mark + word, false));
  const darkBody = (mark + word).replaceAll('class="ink"', `stroke="${palette.chalk}"`)
    .replaceAll('class="ink-fill"', `fill="${palette.chalk}"`)
    .replaceAll('class="stamp"', `stroke="${palette.stampDark}"`)
    .replaceAll('class="stamp-fill"', `fill="${palette.stampDark}"`);
  fs.writeFileSync(path.join(out, "logo-dark.svg"),
    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${width} 96" width="${width}" height="96" role="img" aria-label="postmarkd"><title>postmarkd</title>${darkBody}</svg>\n`);
  return { mark, markSVG: svg(96, 96, mark, false) };
}

async function render(markSVG) {
  const { chromium } = await import("playwright");
  const browser = await chromium.launch(process.env.CHROMIUM ? { executablePath: process.env.CHROMIUM } : {});
  const tpl = fs.readFileSync(path.join(here, "graphics.html"), "utf8")
    .replace("{{MARK}}", markSVG.replace(/<style>.*?<\/style>/, ""));
  for (const [file, query, w, h] of [
    ["banner-light.png", "banner&theme=light", 1280, 400],
    ["banner-dark.png", "banner&theme=dark", 1280, 400],
    ["social-card.png", "card&theme=light", 1280, 640],
  ]) {
    const page = await browser.newPage({ deviceScaleFactor: 2, viewport: { width: w, height: h } });
    page.on("pageerror", e => { throw new Error(`${file}: ${e.message}`); });
    await page.setContent(tpl.replace("{{QUERY}}", query), { waitUntil: "networkidle" });
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({ path: path.join(out, file), omitBackground: false });
    await page.close();
    console.log("wrote", file);
  }
  await browser.close();
}

const { markSVG } = await buildSVGs();
console.log("wrote mark.svg, favicon.svg, logo.svg, logo-light.svg, logo-dark.svg");
if (!process.argv.includes("--svg-only")) await render(markSVG);
