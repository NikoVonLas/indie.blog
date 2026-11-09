// Only explicit UI strings use this dictionary; author content is never translated.
let uiMessages;
window.blogText = (text) => {
  if (document.documentElement.lang !== 'en') return text;
  uiMessages ??= JSON.parse(document.getElementById('ui-text')?.textContent || '{}');
  return uiMessages[text] || text;
};
// Keep navigation available without JavaScript; collapse it only after initialization.
(() => {
  document.documentElement.setAttribute('data-js', '');
  document.addEventListener('DOMContentLoaded', () => {
    const language = document.querySelector('.language-picker');
    language?.querySelector('select').addEventListener('change', () => language.requestSubmit());
    language?.addEventListener('submit', () => {
      language.querySelector('[name="next"]').value = location.pathname + location.search + location.hash;
    });
    const button = document.querySelector('.header-burger');
    const menu = document.querySelector('#header-menu');
    button.addEventListener('click', () => {
      const open = menu.classList.toggle('header-menu-visible');
      button.setAttribute('aria-expanded', String(open));
      button.setAttribute('aria-label', open ? button.dataset.closeLabel : button.dataset.openLabel);
    });
    const searchButton = document.querySelector('.search-toggle');
    const search = document.querySelector('#site-search');
    const query = search.querySelector('input[name="q"]');
    const setSearchOpen = (open) => {
      search.classList.toggle('site-search-visible', open);
      searchButton.setAttribute('aria-expanded', String(open));
      if (open) query.focus();
    };
    searchButton.addEventListener('click', () => {
      setSearchOpen(searchButton.getAttribute('aria-expanded') !== 'true');
    });
    search.addEventListener('keydown', (event) => {
      if (event.key === 'Escape') {
        setSearchOpen(false);
        searchButton.focus();
      }
    });
  });
})();
