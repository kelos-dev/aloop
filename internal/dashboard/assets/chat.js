const transcript = document.querySelector('#conversation');
if (!new URLSearchParams(location.search).has('before')) {
  transcript.scrollTop = transcript.scrollHeight;
  window.setTimeout(refreshChat, 4000);
}

// Replace only the transcript so an incoming answer preserves a draft and its focus.
async function refreshChat() {
  try {
    if (document.visibilityState !== 'visible') return;
    const response = await fetch(location.href, {cache: 'no-store'});
    if (!response.ok) return;
    const page = new DOMParser().parseFromString(await response.text(), 'text/html');
    const next = page.querySelector('#conversation');
    if (next && transcript.innerHTML !== next.innerHTML) {
      const atEnd = transcript.scrollHeight - transcript.clientHeight - transcript.scrollTop < 32;
      transcript.replaceChildren(...next.childNodes);
      if (atEnd) transcript.scrollTop = transcript.scrollHeight;
    }
  } catch {
    // A subsequent poll can recover from a temporary network failure.
  } finally {
    window.setTimeout(refreshChat, 4000);
  }
}
