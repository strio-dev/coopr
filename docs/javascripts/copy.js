(() => {
  const feedback = new WeakMap();

  document.addEventListener("click", async (event) => {
    const button = event.target instanceof Element
      ? event.target.closest('.md-code__button[data-md-type="copy"]')
      : null;
    // Leave the theme's ClipboardJS fallback available in older browsers.
    if (!button || !navigator.clipboard?.writeText) return;

    event.preventDefault();
    event.stopImmediatePropagation();

    let state = feedback.get(button);
    if (!state) {
      state = { title: button.title, label: button.getAttribute("aria-label") };
      feedback.set(button, state);
    }
    clearTimeout(state.timer);
    const attempt = Symbol();
    state.attempt = attempt;
    delete button.dataset.copyState;

    try {
      let text = button.getAttribute("data-clipboard-text");
      if (!text) {
        const target = document.querySelector(button.getAttribute("data-clipboard-target"));
        // Match the theme's rendered-text and annotation-exclusion contract.
        target.setAttribute("data-md-copying", "");
        try {
          const override = target.closest("[data-copy]");
          text = (override ? override.getAttribute("data-copy") : target.innerText).trimEnd();
        } finally {
          target.removeAttribute("data-md-copying");
        }
      }
      await navigator.clipboard.writeText(text);
      if (state.attempt !== attempt) return;
      button.dataset.copyState = "copied";
      button.title = "Copied";
    } catch {
      if (state.attempt !== attempt) return;
      button.dataset.copyState = "error";
      button.title = "Could not copy to clipboard";
      window.alert$.next(button.title);
    }

    button.setAttribute("aria-label", button.title);
    button.focus();
    state.timer = setTimeout(() => {
      delete button.dataset.copyState;
      button.title = state.title;
      if (state.label === null) button.removeAttribute("aria-label");
      else button.setAttribute("aria-label", state.label);
    }, 2000);
  }, true);
})();
