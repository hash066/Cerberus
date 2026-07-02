/* Cerberus website — tiny vanilla JS.
   Three small progressive-enhancement behaviours, no framework, no build step:
     1. copy-to-clipboard on command snippets
     2. OS tab switching on the install page
     3. persistent dark/light theme toggle
   The site is fully usable with JS disabled; this only adds convenience. */

(function () {
  "use strict";

  /* ---- theme toggle ------------------------------------------------------- */
  var root = document.documentElement;
  var stored = null;
  try { stored = localStorage.getItem("cerberus-theme"); } catch (e) { /* private mode */ }
  if (stored === "light" || stored === "dark") {
    root.setAttribute("data-theme", stored);
  }

  function currentTheme() {
    var attr = root.getAttribute("data-theme");
    if (attr) return attr;
    return window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
  }

  function syncToggleLabel(btn) {
    var isDark = currentTheme() === "dark";
    btn.textContent = isDark ? "☼" : "☽"; // sun when dark (offer light), moon when light
    btn.setAttribute("aria-label", isDark ? "Switch to light theme" : "Switch to dark theme");
    btn.setAttribute("title", isDark ? "Switch to light theme" : "Switch to dark theme");
  }

  var toggle = document.querySelector(".theme-toggle");
  if (toggle) {
    syncToggleLabel(toggle);
    toggle.addEventListener("click", function () {
      var next = currentTheme() === "dark" ? "light" : "dark";
      root.setAttribute("data-theme", next);
      try { localStorage.setItem("cerberus-theme", next); } catch (e) { /* ignore */ }
      syncToggleLabel(toggle);
    });
  }

  /* ---- copy-to-clipboard -------------------------------------------------- */
  document.querySelectorAll(".codeblock").forEach(function (block) {
    var pre = block.querySelector("pre");
    if (!pre) return;
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "copy-btn";
    btn.textContent = "Copy";
    btn.setAttribute("aria-label", "Copy code to clipboard");
    block.appendChild(btn);

    btn.addEventListener("click", function () {
      // Copy only the command text, skipping lines marked as comments/output.
      var lines = [];
      pre.querySelectorAll("code").forEach(function (code) {
        code.childNodes.forEach(function (n) {
          if (n.nodeType === 1 && (n.classList.contains("out") || n.classList.contains("cmt"))) return;
          lines.push(n.textContent);
        });
      });
      var text = lines.join("").length ? lines.join("").trim() : pre.innerText.trim();

      var done = function () {
        btn.textContent = "Copied";
        btn.classList.add("copied");
        setTimeout(function () { btn.textContent = "Copy"; btn.classList.remove("copied"); }, 1600);
      };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done, fallbackCopy);
      } else {
        fallbackCopy();
      }
      function fallbackCopy() {
        var ta = document.createElement("textarea");
        ta.value = text;
        ta.style.position = "fixed";
        ta.style.opacity = "0";
        document.body.appendChild(ta);
        ta.select();
        try { document.execCommand("copy"); done(); } catch (e) { /* give up quietly */ }
        document.body.removeChild(ta);
      }
    });
  });

  /* ---- OS install tabs ---------------------------------------------------- */
  document.querySelectorAll("[data-tabs]").forEach(function (group) {
    var tabs = Array.prototype.slice.call(group.querySelectorAll(".tab"));
    var panels = tabs.map(function (t) { return document.getElementById(t.getAttribute("aria-controls")); });

    function select(idx) {
      tabs.forEach(function (t, i) {
        var on = i === idx;
        t.setAttribute("aria-selected", on ? "true" : "false");
        t.tabIndex = on ? 0 : -1;
        if (panels[i]) panels[i].hidden = !on;
      });
    }

    // Default to the visitor's OS when we can detect it.
    var ua = navigator.userAgent;
    var guess = 0;
    if (/Mac/i.test(ua)) guess = tabs.findIndex(function (t) { return /mac/i.test(t.dataset.os); });
    else if (/Linux/i.test(ua) && !/Android/i.test(ua)) guess = tabs.findIndex(function (t) { return /linux/i.test(t.dataset.os); });
    else guess = tabs.findIndex(function (t) { return /win/i.test(t.dataset.os); });
    select(guess >= 0 ? guess : 0);

    tabs.forEach(function (t, i) {
      t.addEventListener("click", function () { select(i); });
      t.addEventListener("keydown", function (e) {
        var next = i;
        if (e.key === "ArrowRight") next = (i + 1) % tabs.length;
        else if (e.key === "ArrowLeft") next = (i - 1 + tabs.length) % tabs.length;
        else return;
        e.preventDefault();
        select(next);
        tabs[next].focus();
      });
    });
  });

  /* ---- docs scrollspy (highlight the section in view) --------------------- */
  var docsNav = document.querySelector(".docs-nav");
  if (docsNav && "IntersectionObserver" in window) {
    var links = {};
    docsNav.querySelectorAll('a[href^="#"]').forEach(function (a) {
      links[a.getAttribute("href").slice(1)] = a;
    });
    var headings = document.querySelectorAll(".docs-content h2[id], .docs-content h3[id]");
    var seen = new Map();
    var io = new IntersectionObserver(function (entries) {
      entries.forEach(function (en) { seen.set(en.target.id, en.isIntersecting ? en.intersectionRatio : 0); });
      var best = null, bestR = 0;
      seen.forEach(function (r, id) { if (r > bestR) { bestR = r; best = id; } });
      if (best) {
        Object.keys(links).forEach(function (id) { links[id].classList.toggle("active", id === best); });
      }
    }, { rootMargin: "-70px 0px -70% 0px", threshold: [0, 0.25, 0.5, 1] });
    headings.forEach(function (h) { io.observe(h); });
  }
})();
