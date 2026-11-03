(() => {
  const form = document.querySelector('.editor, .profile-form');
  if (!form) return;
  const status = document.querySelector('#editor-status');
  const body = form.querySelector('textarea[name="body"]');
  const csrf = form.querySelector('input[name="csrf"]').value;
  let pending = 0;
  const notify = (message) => { status.textContent = message; };
  const insert = (text) => {
    if (!body) return;
    body.setRangeText(text, body.selectionStart, body.selectionEnd, 'end');
    body.dispatchEvent(new Event('input', { bubbles: true }));
    body.focus();
  };
  const request = async (url, data) => {
    const response = await fetch(url, { method: 'POST', headers: { 'X-CSRF-Token': csrf }, body: data });
    if (!response.ok || !response.headers.get('content-type')?.includes('application/json')) {
      throw new Error(response.redirected ? 'Сессия закончилась. Войдите снова.' : await response.text());
    }
    return response.json();
  };
  async function upload(file, target) {
    if (!file) return;
    if (file.size > 10 * 1024 * 1024) { notify('Размер изображения — не больше 10 МБ.'); return; }
    pending++;
    notify('Загружаю изображение…');
    try {
      const data = new FormData(); data.append('image', file);
      const result = await request(`/uploads/?purpose=${target === 'avatar' ? 'avatar' : 'post'}`, data);
      if (target === 'body') {
        const alt = file.name.replace(/\.[^.]+$/, '').replace(/[\[\]\\\r\n]/g, '');
        if (form.querySelector('[name="raw_html"]')?.checked) {
          insert(`\n<img src="${result.url}" alt="">\n`);
        } else insert(`\n![${alt}](${result.url})\n`);
      } else {
        const field = form.querySelector(`[name="${target}"]`);
        field.value = result.url;
        field.dispatchEvent(new Event('input', { bubbles: true }));
      }
      notify('Изображение загружено. Сохраните изменения.');
    } catch (error) { notify(error.message || 'Не удалось загрузить изображение.'); }
    finally { pending--; }
  }
  for (const picker of form.querySelectorAll('[data-upload]')) {
    picker.addEventListener('change', async () => { await upload(picker.files[0], picker.dataset.upload); picker.value = ''; });
  }
  for (const field of form.querySelectorAll('[data-image-preview]')) {
    const preview = document.getElementById(field.dataset.imagePreview);
    const update = () => { preview.hidden = !field.value; if (field.value) preview.src = field.value; else preview.removeAttribute('src'); };
    field.addEventListener('input', update); update();
  }
  form.addEventListener('submit', (event) => {
    if (pending) { event.preventDefault(); notify('Дождитесь завершения загрузки или вставки ссылки.'); }
  });
  const links = form.querySelector('[data-about-links]');
  if (links) {
    const add = form.querySelector('[data-add-about-link]');
    add.hidden = false;
    const update = () => {
      add.disabled = links.children.length >= 12;
      for (const button of links.querySelectorAll('[data-remove-about-link]')) button.hidden = false;
    };
    links.addEventListener('click', (event) => {
      const button = event.target.closest('[data-remove-about-link]');
      if (button) { button.closest('.about-link-row').remove(); update(); }
    });
    add.addEventListener('click', () => {
      if (links.children.length >= 12) return;
      links.append(document.getElementById('about-link-template').content.cloneNode(true));
      update(); links.lastElementChild.querySelector('input').focus();
    });
    update();
  }
  if (!body) return;
  body.addEventListener('paste', (event) => {
    const files = [...event.clipboardData.files].filter(file => file.type.startsWith('image/'));
    if (files.length) { event.preventDefault(); (async () => { for (const file of files) await upload(file, 'body'); })(); }
  });
  body.addEventListener('dragover', (event) => { if (event.dataTransfer.types.includes('Files')) event.preventDefault(); });
  body.addEventListener('drop', (event) => {
    if (!event.dataTransfer.files.length) return;
    event.preventDefault();
    const files = [...event.dataTransfer.files];
    (async () => { for (const file of files) await upload(file, 'body'); })();
  });
  const insertCode = (language, source) => {
    if (form.querySelector('[name="raw_html"]')?.checked) {
      const escaped = source.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
      insert(`\n<pre><code class="language-${language}">${escaped}</code></pre>\n`);
    } else insert(`\n\n\`\`\`${language}\n${source}\n\`\`\`\n\n`);
  };
  form.querySelector('[data-insert="mermaid"]').addEventListener('click', () => insertCode('mermaid', 'flowchart LR\n  A[Идея] --> B[Заметка]\n  B --> C[Статья]'));
  form.querySelector('[data-insert="embed"]').addEventListener('click', () => insertCode('embed', '<button id="hello">Нажми</button>\n<script>document.querySelector("#hello").onclick = function () { this.textContent = "Работает!"; };</script>'));
  form.querySelector('[data-link-preview]').addEventListener('click', async () => {
    const url = form.querySelector('#link-url');
    if (!url.value || !url.checkValidity()) { url.reportValidity(); notify('Введите полный адрес страницы.'); return; }
    pending++; notify('Получаю превью страницы…');
    try {
      const result = await request('/godmode/link-preview/', new URLSearchParams({ url: url.value }));
      insert(result.html); url.value = ''; notify('Карточка ссылки вставлена.');
    } catch (error) { notify(error.message || 'Не удалось получить превью.'); }
    finally { pending--; }
  });
})();
