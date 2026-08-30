// The AUTH cookie is HttpOnly, so document.cookie won't see it.
// Instead, run this in the console to fetch cookies via an API call:

(async () => {
  const resp = await fetch('/api/core/v4/users', { credentials: 'include' });
  const setCookies = resp.headers.get('set-cookie');
  
  // We can't read set-cookie from JS, so use this instead:
  // Open DevTools → Network tab → click any request to protonvpn.com
  // → Request Headers → copy the "cookie:" value
  
  // Alternative: fetch a page and extract from network
  console.log('⚠️ HttpOnly cookies cannot be read from JS.');
  console.log('');
  console.log('Do this instead:');
  console.log('1. Open DevTools (F12) → Network tab');
  console.log('2. Refresh the page (F5)');
  console.log('3. Click on any request to account.protonvpn.com');
  console.log('4. Look at "Request Headers" → find "cookie:"');
  console.log('5. Copy the entire cookie value');
  console.log('');
  console.log('It should look like:');
  console.log('AUTH-xxxx=fajq...; Session-Id=xxxx; Tag=default; ...');
})();
