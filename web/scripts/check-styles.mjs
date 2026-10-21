// Keeps screens on the design system (docs/plans/design-system.md):
// outside src/components/ui, no font size, radius or color is made up on
// the spot. Use the type scale (text-caption … text-display), rounded-control
// or rounded-card, and the color tokens. Sizes relative to the text around
// them (text-[0.9em]) and rounded-[inherit] are fine.
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";

const root = new URL("../src", import.meta.url).pathname;
const rules = [
  [/\btext-\[\d[\d.]*(px|rem)\]/g, "off the type scale (text-caption, -meta, -body-sm, -body, -title-sm, -title, -display)"],
  [/\brounded(-[trbl]{1,2})?-\[\d[^\]]*\]/g, "off the radii (rounded-control, rounded-card, rounded-full)"],
  [/\b(bg|text|border|fill|stroke|ring|outline|from|to|via)-\[#[0-9a-fA-F]+\]/g, "not a color token: add a role to index.css"],
];

function* files(dir) {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) {
      if (p !== join(root, "components", "ui")) yield* files(p);
    } else if (/\.tsx?$/.test(name)) yield p;
  }
}

let bad = 0;
for (const f of files(root)) {
  readFileSync(f, "utf8")
    .split("\n")
    .forEach((line, i) => {
      for (const [re, why] of rules) {
        for (const m of line.matchAll(re)) {
          console.error(`${relative(process.cwd(), f)}:${i + 1}: ${m[0]} is ${why}`);
          bad++;
        }
      }
    });
}
if (bad) {
  console.error(`\n${bad} style ${bad === 1 ? "value is" : "values are"} off the design system.`);
  process.exit(1);
}
