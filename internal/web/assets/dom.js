// The DOM helper every screen and every chart builds with.
//
// Separate from app.js deliberately: app.js registers listeners at module
// scope and cannot be imported outside a browser, while chart.js needs this
// helper AND is unit-tested under node. Keep this module free of top-level
// browser access.

const SVG_TAGS = new Set(["svg","g","path","rect","circle","line","text",
  "polyline","polygon","defs","clipPath","tspan","title"]);

// el builds a DOM node. Children that are strings become TEXT NODES, never
// markup — which is why no part of this app needs to think about escaping a
// model name, an account label, or an error message from an upstream.
export function el(tag, attrs = {}, ...children) {
  const ns = SVG_TAGS.has(tag) ? "http://www.w3.org/2000/svg" : "http://www.w3.org/1999/xhtml";
  const node = document.createElementNS(ns, tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on") && typeof v === "function") node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, String(v));
  }
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue;
    node.appendChild(typeof child === "object" ? child : document.createTextNode(String(child)));
  }
  return node;
}
