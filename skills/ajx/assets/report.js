/* Local report controls. No network calls, dependencies, or generated HTML. */
(() => {
  "use strict";
  document.documentElement.classList.add("has-js");
  function revealTarget() {
    let id;
    try { id = decodeURIComponent(location.hash.slice(1)); } catch { return; }
    const details = document.getElementById(id)?.closest("details");
    if (details) details.open = true;
  }
  window.addEventListener("hashchange", revealTarget);
  revealTarget();
  for (const scope of document.querySelectorAll("[data-filter-scope]")) {
    const query = scope.querySelector("[data-query]");
    const type = scope.querySelector("[data-kind]");
    const sort = scope.querySelector("[data-sort]");
    const list = scope.querySelector("[data-items]");
    if (!list) continue;
    const items = Array.from(list.children).filter(el => el.hasAttribute("data-item"));
    const count = scope.querySelector("[data-count]");
    const empty = scope.querySelector("[data-empty]");
    let compareOnly = false;
    function update() {
      const needle = (query?.value || "").trim().toLocaleLowerCase();
      const kind = type?.value || "";
      let shown = 0;
      for (const item of items) {
        const selected = item.querySelector("[data-select-run]")?.checked ?? true;
        const visible = (!needle || item.textContent.toLocaleLowerCase().includes(needle)) &&
          (!kind || (item.dataset.kinds || "").split("|").includes(kind)) &&
          (!compareOnly || selected);
        item.hidden = !visible;
        if (visible) shown++;
      }
      const key = sort?.value || "order";
      const ordered = items.slice().sort((a, b) => {
        if (key === "order") return Number(a.dataset.order) - Number(b.dataset.order);
        const av = a.dataset[key], bv = b.dataset[key];
        if (!av && !bv) return Number(a.dataset.order) - Number(b.dataset.order);
        if (!av) return 1;
        if (!bv) return -1;
        return Number(bv) - Number(av) || Number(a.dataset.order) - Number(b.dataset.order);
      });
      for (const item of ordered) list.appendChild(item);
      if (count) count.textContent = `${shown} of ${items.length} ${scope.dataset.filterScope}`;
      if (empty) empty.hidden = shown !== 0;
    }
    query?.addEventListener("input", update);
    type?.addEventListener("change", update);
    sort?.addEventListener("change", update);
    scope.querySelector("[data-compare]")?.addEventListener("click", () => {
      compareOnly = true;
      update();
    });
    scope.querySelector("[data-reset]")?.addEventListener("click", () => {
      if (query) query.value = "";
      if (type) type.value = "";
      if (sort) sort.value = "order";
      for (const item of items) {
        const checkbox = item.querySelector("[data-select-run]");
        if (checkbox) checkbox.checked = true;
      }
      compareOnly = false;
      update();
    });
    scope.addEventListener("change", event => {
      if (event.target.matches("[data-select-run]") && compareOnly) update();
    });
    update();
  }
})();
