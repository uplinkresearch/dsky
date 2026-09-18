// A browser, to the extent this page needs one.
//
// Deliberately generic: unknown ids and selectors return a working element
// rather than null, so the page can be rearranged without rewriting this. What
// it must NOT do is swallow errors — the whole point is that an exception
// anywhere in the page's own code reaches the test.
"use strict";
const listeners = [];
class Elem {
  constructor(tag) {
    this.tagName = (tag || "div").toUpperCase();
    this.children = []; this.attrs = {}; this.style = {}; this.dataset = {};
    this._text = ""; this.className = ""; this.hidden = false; this.value = "";
    this.classList = {
      _s: new Set(),
      add: (...c) => c.forEach(x => this.classList._s.add(x)),
      remove: (...c) => c.forEach(x => this.classList._s.delete(x)),
      toggle: (c, on) => on ? this.classList._s.add(c) : this.classList._s.delete(c),
      contains: c => this.classList._s.has(c),
    };
  }
  get textContent() {
    return this._text + this.children.map(c => c.textContent || "").join("");
  }
  set textContent(v) { this._text = v == null ? "" : String(v); this.children = []; }
  get firstChild() { return this.children[0] || null; }
  get childElementCount() { return this.children.length; }
  get parentElement() { return this._parent || null; }
  append(...n) { for (const c of n) { const e = toNode(c); e._parent = this; this.children.push(e); } }
  appendChild(c) { this.append(c); return c; }
  prepend(...n) { for (const c of n.reverse()) { const e = toNode(c); e._parent = this; this.children.unshift(e); } }
  replaceChildren(...n) { this.children = []; this.append(...n); }
  remove() { const p = this._parent; if (p) p.children = p.children.filter(c => c !== this); }
  removeChild(c) { this.children = this.children.filter(x => x !== c); return c; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  removeAttribute(k) { delete this.attrs[k]; }
  hasAttribute(k) { return k in this.attrs; }
  addEventListener(t, f) { listeners.push([this, t, f]); this["on" + t] = f; }
  removeEventListener() {}
  dispatchEvent(ev) {
    const f = this["on" + ev.type];
    if (typeof f === "function") f.call(this, ev);
    for (const [el, t, g] of listeners) if (el === this && t === ev.type) g.call(this, ev);
    return true;
  }
  // Every element is findable; the page is allowed to look for anything.
  querySelector() { return new Elem("div"); }
  querySelectorAll() { return []; }
  closest() { return new Elem("div"); }
  focus() {} blur() {} click() { this.dispatchEvent(new Ev("click")); }
  scrollIntoView() {} getBoundingClientRect() { return { top: 0, left: 0, width: 100, height: 20, bottom: 20, right: 100 }; }
  // Canvas: the page checks for a context and skips its decoration when there
  // is none, which is what should happen here.
  getContext() { return null; }
  get innerHTML() { return ""; } set innerHTML(v) { this._text = ""; this.children = []; }
}
function toNode(c) { return typeof c === "string" ? Object.assign(new Elem("span"), { _text: c }) : c; }
class Ev { constructor(type, init) { Object.assign(this, init || {}); this.type = type; }
  preventDefault() {} stopPropagation() {} }

const byId = new Map();
globalThis.document = {
  createElement: t => new Elem(t),
  createElementNS: (ns, t) => new Elem(t),
  createTextNode: t => toNode(String(t)),
  getElementById: id => { if (!byId.has(id)) byId.set(id, new Elem("div")); return byId.get(id); },
  querySelector: () => new Elem("div"),
  querySelectorAll: () => [],
  addEventListener: (t, f) => listeners.push([globalThis.document, t, f]),
  body: new Elem("body"),
  documentElement: new Elem("html"),
  head: new Elem("head"),
  title: "",
  hidden: false,
  visibilityState: "visible",
};
Object.defineProperty(globalThis, "window", { value: globalThis, configurable: true });
globalThis.Event = Ev; globalThis.CustomEvent = Ev;
Object.defineProperty(globalThis, "location", { configurable: true, value: { href: "http://127.0.0.1:8931/#t=testtoken", hash: "#t=testtoken",
  origin: "http://127.0.0.1:8931", pathname: "/", reload() {}, replace() {}, assign() {} } });
globalThis.history = { replaceState() {}, pushState() {} };
// navigator is read-only in modern node, so it is defined rather than assigned.
Object.defineProperty(globalThis, "navigator", { value: { clipboard: { writeText: async () => {} }, userAgent: "node", platform: "test" }, configurable: true });
globalThis.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
globalThis.matchMedia = () => ({ matches: false, addEventListener() {}, addListener() {} });
globalThis.getComputedStyle = () => ({ getPropertyValue: () => "" });
globalThis.requestAnimationFrame = f => { globalThis.__raf = f; return 1; };
globalThis.cancelAnimationFrame = () => {};
// Timers do nothing: this test is about the page loading, not about what it
// does a minute later, and a live interval would hold node open.
globalThis.setInterval = () => 0;
globalThis.setTimeout = () => 0;
globalThis.clearInterval = () => {}; globalThis.clearTimeout = () => {};
globalThis.EventSource = class { constructor() { this.readyState = 0; } close() {} addEventListener() {} };
globalThis.alert = () => {}; globalThis.confirm = () => true;
// window's own listeners, which the page registers bare.
globalThis.addEventListener = (t, f) => listeners.push([globalThis, t, f]);
globalThis.removeEventListener = () => {};
globalThis.dispatchEvent = () => true;
globalThis.scrollTo = () => {}; globalThis.focus = () => {};

// The server, as far as the page is concerned. Anything not listed answers {}
// rather than failing: the test is about the page's own code.
const FIXTURE = JSON.parse(process.argv[2]);
globalThis.fetch = async (url) => {
  const path = String(url).split("?")[0];
  const body = FIXTURE[path] !== undefined ? FIXTURE[path] : {};
  return { ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body),
           headers: { get: () => "application/json" } };
};
