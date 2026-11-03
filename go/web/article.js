import hljs from 'highlight.js/lib/common';

for (const code of document.querySelectorAll('.prose pre > code')) {
  if (!code.classList.contains('language-mermaid') && !code.classList.contains('language-embed')) {
    const language = [...code.classList].find(c => c.startsWith('language-'))?.slice(9);
    if (language && hljs.getLanguage(language)) hljs.highlightElement(code);
  }
}

const diagrams = [...document.querySelectorAll('.prose pre > code.language-mermaid')];
if (diagrams.length) {
  try {
    const { default: mermaid } = await import('mermaid');
    mermaid.initialize({
      startOnLoad: false, securityLevel: 'strict', theme: 'dark', look: 'handDrawn',
      handDrawnSeed: 42, fontFamily: '"Comic Sans MS", "Ubuntu", sans-serif',
      htmlLabels: false, flowchart: { htmlLabels: false }, maxTextSize: 50000, maxEdges: 500,
      secure: ['securityLevel', 'startOnLoad', 'maxTextSize', 'maxEdges', 'htmlLabels', 'look'],
      suppressErrorRendering: true,
    });
    for (const [index, code] of diagrams.entries()) {
      const source = code.textContent;
      const figure = document.createElement('figure');
      figure.className = 'diagram';
      const picture = document.createElement('div');
      picture.className = 'diagram-picture';
      picture.setAttribute('role', 'img');
      picture.setAttribute('aria-label', 'Диаграмма Mermaid');
      try {
        const { svg } = await mermaid.render(`diagram-${index}`, source);
        picture.innerHTML = svg;
        const details = document.createElement('details');
        const summary = document.createElement('summary');
        summary.textContent = 'Код диаграммы';
        details.append(summary);
        const pre = code.parentElement;
        pre.replaceWith(figure);
        details.append(pre);
        figure.append(picture, details);
      } catch {
        const message = document.createElement('p');
        message.className = 'hint';
        message.textContent = 'Не удалось построить диаграмму. Проверьте синтаксис Mermaid ниже.';
        code.parentElement.before(message);
      }
    }
  } catch {
    // The original, readable Mermaid source remains available if loading fails.
  }
}
