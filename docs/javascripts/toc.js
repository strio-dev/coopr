(() => {
  let toc;
  let anchors = [];
  let target;
  let targetScroll;
  let frame;

  function update() {
    frame = undefined;
    if (!toc || !anchors.length) return;

    const y = Math.max(0, window.scrollY);
    const height = window.innerHeight;
    const end = Math.max(0, document.scrollingElement.scrollHeight - height);
    const main = document.querySelector("main");
    const inset = main.firstElementChild.getBoundingClientRect().top - main.getBoundingClientRect().top;
    const header = document.querySelector(".md-header").offsetHeight;
    const offset = header + .9 * inset;
    const last = anchors[anchors.length - 1].heading.getBoundingClientRect();
    const start = Math.max(0, end - height);
    const adjusted = end > 0 && last.top + y > end + offset && y > start;
    toc.toggleAttribute("data-coopr-toc-adjusted", adjusted);

    let active;
    if (adjusted) {
      // Move the reading line through otherwise unreachable headings in the
      // final viewport, without adding empty space or changing native classes.
      const progress = Math.min(1, (y - start) / (end - start));
      const line = y + offset + progress * (last.bottom + y - end - offset);
      for (const anchor of anchors) {
        if (anchor.heading.getBoundingClientRect().top + y <= line) active = anchor;
      }

      // Preserve the requested heading once its anchor scroll settles. Several
      // late anchors can reach the same clamped scroll position.
      if (target) {
        const position = target.heading.getBoundingClientRect();
        const margin = parseFloat(getComputedStyle(target.heading).scrollMarginTop);
        const padding = parseFloat(getComputedStyle(document.documentElement).scrollPaddingTop) || 0;
        const destination = Math.min(end, Math.max(0, position.top + y - margin - padding));
        if (Math.abs(y - destination) <= 1 && position.bottom > header && position.top < height) {
          active = target;
          targetScroll = y;
        }
      }
    }
    for (const anchor of anchors) {
      if (anchor === active) anchor.link.setAttribute("aria-current", "location");
      else anchor.link.removeAttribute("aria-current");
    }
  }

  function schedule() {
    if (frame === undefined) frame = requestAnimationFrame(update);
  }

  function selectTarget(hash) {
    target = anchors.find(anchor => anchor.link.hash === hash);
    targetScroll = undefined;
    schedule();
  }

  function releaseTarget() {
    target = undefined;
    targetScroll = undefined;
    schedule();
  }

  document$.subscribe(() => {
    toc = document.querySelector(".md-sidebar--secondary .md-nav--secondary");
    anchors = toc ? [...toc.querySelectorAll("a.md-nav__link")].map(link => ({
      link,
      heading: document.getElementById(decodeURIComponent(link.hash.slice(1))),
    })).filter(anchor => anchor.heading) : [];
    selectTarget(window.location.hash);
  });

  document.addEventListener("click", event => {
    const link = event.target instanceof Element
      ? event.target.closest("a[href]") : null;
    if (link && !event.ctrlKey && !event.metaKey && !event.altKey && !event.shiftKey
        && link.origin === location.origin && link.pathname === location.pathname
        && link.search === location.search && link.hash) selectTarget(link.hash);
  });
  window.addEventListener("hashchange", () => selectTarget(window.location.hash));
  window.addEventListener("popstate", () => selectTarget(window.location.hash));
  window.addEventListener("scroll", () => {
    if (targetScroll !== undefined && Math.abs(window.scrollY - targetScroll) > 1) releaseTarget();
    schedule();
  }, { passive: true });
  window.addEventListener("resize", releaseTarget);
  window.addEventListener("wheel", releaseTarget, { passive: true });
  window.addEventListener("touchmove", releaseTarget, { passive: true });
  document.addEventListener("keydown", event => {
    if (!event.defaultPrevented && ["ArrowUp", "ArrowDown", "PageUp", "PageDown", "Home", "End", " "].includes(event.key)
        && !event.target.closest("input, textarea, select, [contenteditable]")) releaseTarget();
  });
  new ResizeObserver(schedule).observe(document.body);
})();
