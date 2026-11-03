(() => {
  const root = document.querySelector('[data-passkey-mode]');
  if (!root) return;
  const status = document.getElementById('passkey-status');
  const login = document.getElementById('passkey-login');
  const register = document.getElementById('passkey-register');
  let setupToken = '';
  if (root.dataset.passkeyMode === 'admin-setup') {
    setupToken = location.hash.slice(1);
    // The setup secret is never sent in a URL or retained in browser history.
    history.replaceState(null, '', location.pathname);
    if (/^[a-f0-9]{64}$/.test(setupToken)) {
      register.disabled = false;
      document.getElementById('passkey-setup-hint').textContent = 'Ссылка действует 15 минут и подходит для регистрации одного ключа.';
    } else setupToken = '';
  }
  if (!window.isSecureContext || !window.PublicKeyCredential) {
    status.textContent = 'Passkey недоступен. Откройте сайт по HTTPS (для локального просмотра — localhost) в браузере с поддержкой Passkey.';
    if (login) login.disabled = true;
    if (register) register.disabled = true;
    return;
  }
  const decode = (value) => Uint8Array.from(atob(value.replace(/-/g, '+').replace(/_/g, '/')), c => c.charCodeAt(0));
  const encode = (buffer) => btoa(String.fromCharCode(...new Uint8Array(buffer))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  const optionsFromJSON = (data) => {
    const options = data.publicKey;
    options.challenge = decode(options.challenge);
    if (options.user) options.user.id = decode(options.user.id);
    for (const property of ['allowCredentials', 'excludeCredentials']) {
      if (options[property]) options[property] = options[property].map(c => ({...c, id: decode(c.id)}));
    }
    return options;
  };
  const credentialJSON = (credential) => {
    const response = {};
    for (const key of ['clientDataJSON', 'attestationObject', 'authenticatorData', 'signature', 'userHandle']) {
      if (credential.response[key]) response[key] = encode(credential.response[key]);
    }
    if (credential.response.getTransports) response.transports = credential.response.getTransports();
    return {id: credential.id, rawId: encode(credential.rawId), type: credential.type, response,
      authenticatorAttachment: credential.authenticatorAttachment,
      clientExtensionResults: credential.getClientExtensionResults()};
  };
  const post = async (path, body) => {
    const response = await fetch(path, {method: 'POST', credentials: 'same-origin', headers: {
      'Content-Type': 'application/json', 'X-CSRF-Token': document.getElementById('passkey-csrf').value,
    }, body: JSON.stringify(body)});
    let result;
    try { result = await response.json(); } catch { throw new Error('Не удалось выполнить запрос. Обновите страницу и попробуйте ещё раз.'); }
    if (!response.ok) throw new Error(result.error || 'Не удалось выполнить запрос.');
    return result;
  };
  const run = async (button, registering) => {
    button.disabled = true;
    status.textContent = 'Подтвердите действие на своём устройстве…';
    try {
      const path = '/admin/passkey/' + (registering ? 'register' : 'login');
      const input = registering ? {token: setupToken, first: root.dataset.firstKey === 'true', label: document.getElementById('passkey-label')?.value || 'Основной ключ'} : {next: root.dataset.next};
      const options = optionsFromJSON(await post(path + '/begin', input));
      const credential = registering ? await navigator.credentials.create({publicKey: options}) : await navigator.credentials.get({publicKey: options});
      if (!credential) throw new Error('Ключ не выбран. Попробуйте ещё раз.');
      const result = await post(path + '/finish', credentialJSON(credential));
      setupToken = '';
      if (registering) {
        status.textContent = result.message;
        const link = document.createElement('a');
        link.href = '/admin?reauth=1'; link.textContent = 'Войти с новым ключом';
        status.append(document.createElement('br'), link);
      } else location.assign(result.redirect);
    } catch (error) {
      status.textContent = error.name === 'NotAllowedError' ? 'Действие отменено или время ожидания истекло. Нажмите кнопку, чтобы попробовать ещё раз.' : error.name === 'InvalidStateError' ? 'Этот ключ уже зарегистрирован. Используйте его для входа или выберите другой.' : error.message;
      button.disabled = false;
    }
  };
  if (login) login.addEventListener('click', () => run(login, false));
  if (register) register.addEventListener('click', () => run(register, true));
})();
