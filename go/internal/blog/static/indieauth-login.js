// Navigate outside the form submission so CSP can keep form-action 'self'.
const continuation = document.getElementById('indieauth-continue');
if (continuation) window.location.replace(continuation.href);
