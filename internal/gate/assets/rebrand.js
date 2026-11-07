// Lumen rebrand for DeepSeek Harness. Injected by the Lumen gate.
// Renames visible "DeepSeek Harness" text to "Lumen" and hides the
// decorative home-screen canvas (DSH's click-triggered whale). It never
// touches inputs, the conversation content, or anything the user typed.
(() => {
  "use strict";
  const NAME_RE = /DeepSeek Harness|DeepSeek|\bDSH\b/g;
  const SKIP = new Set(["SCRIPT", "STYLE", "TEXTAREA", "INPUT", "CODE", "PRE"]);

  function inUserContent(el) {
    // Don't rewrite messages, the composer, or editable areas.
    return !!el.closest('[contenteditable="true"], [data-turn-tail], [class*="_message"], [class*="_markdown"], [class*="_composer"]');
  }

  function renameText(root) {
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (let n = walker.nextNode(); n; n = walker.nextNode()) {
      const p = n.parentElement;
      if (!p || SKIP.has(p.tagName) || inUserContent(p)) continue;
      if (NAME_RE.test(n.nodeValue)) {
        NAME_RE.lastIndex = 0;
        n.nodeValue = n.nodeValue.replace(NAME_RE, "Lumen");
      }
      NAME_RE.lastIndex = 0;
    }
    for (const el of root.querySelectorAll ? root.querySelectorAll("[title],[aria-label],[placeholder]") : []) {
      for (const a of ["title", "aria-label", "placeholder"]) {
        const v = el.getAttribute(a);
        if (v && NAME_RE.test(v)) el.setAttribute(a, v.replace(NAME_RE, "Lumen"));
        NAME_RE.lastIndex = 0;
      }
    }
  }

  function hideDecorativeCanvas(root) {
    for (const c of root.querySelectorAll ? root.querySelectorAll("canvas:not([data-lumen-hidden])") : []) {
      // The whale canvas is purely decorative: no pointer events, large, in the home view.
      const cs = getComputedStyle(c);
      if (cs.pointerEvents === "none" && c.closest('[class*="_hero"], [class*="_home"], [class*="_frame"], [class*="_background"]')) {
        c.setAttribute("data-lumen-hidden", "");
      }
    }
  }

  function fixTitle() {
    if (NAME_RE.test(document.title)) document.title = document.title.replace(NAME_RE, "Lumen");
    NAME_RE.lastIndex = 0;
  }

  let queued = false;
  function sweep() {
    queued = false;
    fixTitle();
    renameText(document.body);
    hideDecorativeCanvas(document.body);
  }
  function schedule() {
    if (!queued) { queued = true; requestAnimationFrame(sweep); }
  }

  const start = () => {
    sweep();
    new MutationObserver(schedule).observe(document.documentElement, { subtree: true, childList: true, characterData: true });
  };
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start);
  else start();
})();
