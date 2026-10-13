// postmarkd site: theme, tabs, copy buttons, search, navigation.
(() => {
  const $ = (s, el = document) => el.querySelector(s);
  const $$ = (s, el = document) => [...el.querySelectorAll(s)];
  const root = document.documentElement;

  // Theme toggle. The saved choice is applied in <head> before paint.
  const prefersDark = matchMedia("(prefers-color-scheme: dark)");
  const isDark = () => root.dataset.theme ? root.dataset.theme === "dark" : prefersDark.matches;
  $$(".theme-btn").forEach(b => b.addEventListener("click", () => {
    root.dataset.theme = isDark() ? "light" : "dark";
    try { localStorage.setItem("theme", root.dataset.theme); } catch (e) {}
  }));

  // Tabs: each group of [role=tab] buttons switches sibling [data-panel]s.
  $$(".tabs").forEach(tabs => {
    const box = tabs.closest(".term, .install-tabs");
    const buttons = $$("button[data-tab]", tabs);
    const select = btn => {
      buttons.forEach(b => b.setAttribute("aria-selected", String(b === btn)));
      $$("[data-panel]", box).forEach(p => { p.hidden = p.dataset.panel !== btn.dataset.tab; });
    };
    buttons.forEach((b, i) => {
      b.addEventListener("click", () => select(b));
      b.addEventListener("keydown", e => {
        const d = e.key === "ArrowRight" ? 1 : e.key === "ArrowLeft" ? -1 : 0;
        if (d) { const n = buttons[(i + d + buttons.length) % buttons.length]; n.focus(); select(n); }
      });
    });
  });

  // Copy buttons on code blocks.
  $$(".code .copy").forEach(btn => btn.addEventListener("click", async () => {
    const code = $("pre", btn.closest(".code")).innerText.replace(/\n$/, "");
    try {
      await navigator.clipboard.writeText(code);
      btn.textContent = "Copied"; btn.classList.add("done");
    } catch (e) { btn.textContent = "Press ⌘C"; }
    setTimeout(() => { btn.textContent = "Copy"; btn.classList.remove("done"); }, 1600);
  }));

  // Mobile navigation.
  const sidebar = $("#sidebar"), scrim = $(".scrim"), menuBtn = $(".menu-btn");
  const setMenu = open => {
    if (!sidebar) return;
    sidebar.classList.toggle("open", open);
    if (scrim) scrim.hidden = !open;
    if (menuBtn) menuBtn.setAttribute("aria-expanded", String(open));
  };
  menuBtn?.addEventListener("click", () => setMenu(!sidebar.classList.contains("open")));
  scrim?.addEventListener("click", () => setMenu(false));
  $("#sidebar [aria-current=page]")?.scrollIntoView({ block: "center" });

  // Highlight the current section in "On this page".
  const tocLinks = $$(".toc a");
  if (tocLinks.length && "IntersectionObserver" in window) {
    const byId = new Map(tocLinks.map(a => [decodeURIComponent(a.hash.slice(1)), a]));
    const visible = new Set();
    const heads = [...byId.keys()].map(id => document.getElementById(id)).filter(Boolean);
    const update = () => {
      const first = heads.find(h => visible.has(h)) || heads.filter(h => h.getBoundingClientRect().top < 120).pop();
      tocLinks.forEach(a => a.classList.toggle("active", Boolean(first) && a === byId.get(first.id)));
    };
    const io = new IntersectionObserver(entries => {
      entries.forEach(e => e.isIntersecting ? visible.add(e.target) : visible.delete(e.target));
      update();
    }, { rootMargin: "-64px 0px -65% 0px" });
    heads.forEach(h => io.observe(h));
  }

  // Search.
  const dialog = $("dialog.search");
  if (!dialog) return;
  const input = $("input", dialog), list = $(".search-results", dialog), empty = $(".search-empty", dialog);
  const base = input.dataset.root || "";
  let index = null, selected = 0, results = [];

  const load = async () => {
    if (index) return index;
    const res = await fetch(base + "assets/search.json");
    index = (await res.json()).map(e => ({ ...e, hay: (e.t + " " + (e.h || "") + " " + e.x).toLowerCase() }));
    return index;
  };
  const esc = s => s.replace(/[&<>"]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const highlight = (text, terms) => {
    let out = esc(text);
    terms.forEach(t => { if (t.length > 1) out = out.replace(new RegExp(`(${t.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")})`, "gi"), "<mark>$1</mark>"); });
    return out;
  };
  const snippet = (text, terms) => {
    const lower = text.toLowerCase();
    const at = Math.max(0, Math.min(...terms.map(t => { const i = lower.indexOf(t); return i < 0 ? Infinity : i; })) - 40);
    return (at > 0 ? "…" : "") + text.slice(isFinite(at) ? at : 0, (isFinite(at) ? at : 0) + 160);
  };
  const run = async () => {
    const q = input.value.trim().toLowerCase();
    const terms = q.split(/\s+/).filter(Boolean);
    if (!terms.length) { list.innerHTML = ""; empty.hidden = true; return; }
    const idx = await load();
    results = idx.map(e => {
      if (!terms.every(t => e.hay.includes(t))) return null;
      let score = 0;
      for (const t of terms) {
        if (e.t.toLowerCase().includes(t)) score += 6;
        if ((e.h || "").toLowerCase().includes(t)) score += 4;
        score += Math.min(3, e.x.toLowerCase().split(t).length - 1);
      }
      return { e, score };
    }).filter(Boolean).sort((a, b) => b.score - a.score).slice(0, 12);
    selected = 0;
    empty.hidden = results.length > 0;
    list.innerHTML = results.map(({ e }, i) =>
      `<li><a href="${base}${e.u}" role="option" aria-selected="${i === 0}">
        <span class="r-title">${highlight(e.h || e.t, terms)}${e.h ? ` <small>· ${esc(e.t)}</small>` : ""}</span>
        <span class="r-text">${highlight(snippet(e.x, terms), terms)}</span></a></li>`).join("");
  };
  const move = d => {
    const links = $$("a", list);
    if (!links.length) return;
    selected = (selected + d + links.length) % links.length;
    links.forEach((a, i) => a.setAttribute("aria-selected", String(i === selected)));
    links[selected].scrollIntoView({ block: "nearest" });
  };
  const open = () => { if (!dialog.open) { dialog.showModal(); input.select(); load(); } };

  $$("[data-search-open]").forEach(b => b.addEventListener("click", open));
  input.addEventListener("input", run);
  input.addEventListener("keydown", e => {
    if (e.key === "ArrowDown") { e.preventDefault(); move(1); }
    if (e.key === "ArrowUp") { e.preventDefault(); move(-1); }
    if (e.key === "Enter") { const a = $$("a", list)[selected]; if (a) location.href = a.href; }
  });
  dialog.addEventListener("click", e => { if (e.target === dialog) dialog.close(); });
  list.addEventListener("click", () => dialog.close());
  document.addEventListener("keydown", e => {
    const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement?.tagName);
    if ((e.key === "/" && !typing) || (e.key.toLowerCase() === "k" && (e.metaKey || e.ctrlKey))) { e.preventDefault(); open(); }
  });
})();
