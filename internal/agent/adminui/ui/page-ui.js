// Page-owned semantic controls. No selectors or executable code cross the wire.
(() => {
  const root = document.getElementById('page-content');
  const button = document.getElementById('shape-button');
  const dialog = document.getElementById('shape-dialog');
  const toggle = document.getElementById('shape-toggle');
  const registry = [];
  const original = new Map();
  let shaped = false, stop;
  function apply() {
    root.inert = shaped;
    root.classList.toggle('shape-only', shaped);
    if (shaped) {
      root.querySelectorAll('input,textarea').forEach(field => {
        if (!original.has(field)) original.set(field, field.readOnly);
        field.readOnly = true;
      });
    } else {
      original.forEach((value, field) => { field.readOnly = value; });
      original.clear();
    }
    toggle.textContent = shaped ? 'Restore controls' : 'Show only Shape';
    return {shape_only: shaped};
  }
  function shape(value) {
    if (value != null && typeof value !== 'boolean') throw new Error('Expected boolean or null');
    shaped = value == null ? !shaped : value;
    return apply();
  }
  function open(value) {
    if (value != null && typeof value !== 'boolean') throw new Error('Expected boolean or null');
    const show = value == null ? !dialog.open : value;
    if (show && !dialog.open) dialog.showModal();
    if (!show && dialog.open) dialog.close();
    return {open: dialog.open};
  }
  function register(name, description, run) {
    registry.push(BashyPage.register(name, {description, run}));
  }
  function reset() {
    if (stop) stop();
    stop = undefined;
    registry.splice(0).forEach(unregister => unregister());
    shaped = false;
    apply();
    open(false);
    button.hidden = true;
  }
  new MutationObserver(() => { if (shaped) apply(); }).observe(root, {childList: true, subtree: true});
  button.onclick = () => BashyPage.invoke('outpost.shape.open', true);
  document.getElementById('shape-close').onclick = () => BashyPage.invoke('outpost.shape.open', false);
  toggle.onclick = () => BashyPage.invoke('outpost.shape-only', null);
  document.getElementById('shape-navigation').onchange = event => BashyPage.invoke('outpost.navigate', event.target.value);
  window.OutpostPage = {
    reset, register,
    begin() {
      const previous = shaped;
      reset();
      shaped = previous;
      button.hidden = false;
      register('outpost.shape.open', 'Open or close Shape (boolean; null toggles)', open);
      register('outpost.shape-only', 'Show only Shape, preserving field state (boolean; null toggles)', shape);
    },
    connect() {
      apply();
      const url = new URL('api/page/events', document.baseURI);
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
      stop = BashyPage.connect({url: url.href, label: 'Outpost admin'});
    }
  };
})();
