// This Worker has no CMS origin, database, credentials or runtime renderer.
// Only explicitly declared routes can reach its ASSETS binding.
const reserved = ['/cm', '/api', '/mcp', '/.__lc_delivery'];
const safePath = path => typeof path === 'string' && path.startsWith('/') && !path.startsWith('//') && !/[\\\x00%?#]/.test(path) && !path.split('/').some(p => p === '.' || p === '..') && !path.includes('//');
const isReserved = path => reserved.some(prefix => path === prefix || path.startsWith(prefix + '/'));
const owns = (map, key) => Object.hasOwn(map, key);

export function createHandler(input) {
  // Freeze a JSON copy so callers cannot mutate a live routing table.
  const routes = JSON.parse(JSON.stringify(input));
  const pages = routes.pages ?? {};
  const aliases = routes.aliases ?? {};
  const assets = routes.assets ?? {};
  for (const [path, entry] of Object.entries(pages)) {
    if (!safePath(path) || isReserved(path) || path === '/_lc-assets' || path.startsWith('/_lc-assets/') || !safePath(entry.file) || !entry.sha256) throw new Error('Invalid page route');
  }
  for (const [path, entry] of Object.entries(assets)) {
    if (!safePath(path) || !path.startsWith('/_lc-assets/') || !entry.sha256 || !entry.mime) throw new Error('Invalid asset route');
  }
  for (const [path, destination] of Object.entries(aliases)) {
    if (!safePath(path) || !safePath(destination) || isReserved(path) || owns(pages, path) || owns(assets, path) || !owns(pages, destination)) throw new Error('Ambiguous alias');
  }
  function headers(cache = 'no-store') {
    const h = new Headers({'Cache-Control':cache, 'X-Content-Type-Options':'nosniff', 'X-LC-Release':String(routes.release_id ?? '')});
    if (routes.environment === 'test') h.set('X-Robots-Tag','noindex, nofollow');
    return h;
  }
  const failure = (status, message, head = false) => new Response(head ? null : message, {status, headers:headers()});
  return {
    async fetch(request, env) {
      const head = request.method === 'HEAD';
      if (!head && request.method !== 'GET') {
        const h=headers();h.set('Allow','GET, HEAD');
        return new Response('Method not allowed',{status:405,headers:h});
      }
      const url = new URL(request.url);
      let path;
      try { path=decodeURIComponent(url.pathname); } catch { return failure(404,'Not found',head); }
      if (/%(?:2f|5c|00)/i.test(url.pathname) || !safePath(path)) return failure(404,'Not found',head);
      if (path === '/.__lc_delivery/health') {
        const h=headers();h.set('Content-Type','application/json; charset=utf-8');
        const body={protocol:routes.protocol ?? 1,installation_id:routes.installation_id,target_id:routes.target_id,release_id:routes.release_id};
        return new Response(head ? null : JSON.stringify(body),{headers:h});
      }
      if (isReserved(path)) return failure(404,'Not found',head);
      if (owns(aliases,path)) {
        const h=headers();h.set('Location',encodeURI(aliases[path])+url.search);
        return new Response(null,{status:308,headers:h});
      }
      const page = owns(pages,path) ? pages[path] : null;
      const asset = owns(assets,path) ? assets[path] : null;
      if (!page && !asset) return failure(404,'Not found',head);
      const entry = page ?? asset;
      const h=headers(page ? 'no-cache' : 'public,max-age=31536000,immutable');
      h.set('Content-Type',page ? 'text/html; charset=utf-8' : entry.mime);
      const etag='"'+entry.sha256+'"';h.set('ETag',etag);
      // Verify the ASSETS object is actually present before returning a success
      // or conditional hit. Do not pass Range or If-* through to a different URL.
      const assetURL=new URL(url.origin);assetURL.pathname=page ? entry.file : path;
      let response;
      try { response=await env.ASSETS.fetch(new Request(assetURL,{method:head ? 'HEAD' : 'GET'})); }
      catch { return failure(502,'Static asset unavailable',head); }
      if (response.status !== 200) return failure(502,'Static asset unavailable',head);
      const conditional=request.headers.get('If-None-Match');
      if (conditional && conditional.split(',').some(v => v.trim()==='*' || v.trim().replace(/^W\//,'')===etag)) {
        if(response.body) await response.body.cancel();
        return new Response(null,{status:304,headers:h});
      }
      if(head && response.body) await response.body.cancel();
      return new Response(head ? null : response.body,{status:200,headers:h});
    }
  };
}
