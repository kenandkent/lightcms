"""Emit apply_patch input for the eight self-contained LightCMS templates.

Run with a slug to regenerate its HTML, import payload and illustrative data.
This generator prints changes; it never overwrites repository files itself.
"""
import argparse
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
THEMES = {
    "cybersecurity-report": ("Sentinel Brief", "哨兵简报", "Cybersecurity report", "网络安全报告", "Security intelligence / Research desk", "安全情报 / 研究团队", "#0b171c", "#e6f4f0", "#a8beb9", "#263e43", "#73efbb", "01 / INTELLIGENCE", "01 / 安全情报", "⌁"),
    "fund-research": ("Meridian Research", "子午线研究", "Fund research", "基金研究", "Asset allocation / Long-term perspective", "资产配置 / 长期视角", "#f5f6f0", "#173b32", "#53645b", "#d1d9cf", "#29634c", "INVESTMENT RESEARCH", "投资研究", "M"),
    "venture-research": ("Northstar Review", "北辰观察", "Venture research", "创投研究", "Founders, markets and the next cycle", "创始人、市场与下一个周期", "#f4f0eb", "#302447", "#706979", "#d9d2df", "#a84220", "THE VENTURE NOTE", "创投观察", "N"),
    "binance-announcement-style": ("Exchange Bulletin", "交易所简报", "Exchange announcement", "交易所公告", "Product updates / Market notices", "产品更新 / 市场公告", "#fff", "#202630", "#65717d", "#e6e8eb", "#916500", "ANNOUNCEMENTS", "公告中心", "◆"),
    "okx-announcement-style": ("Market Notices", "市场公告", "Exchange notice", "交易所通知", "Updates made clear", "清晰传递更新", "#fff", "#111", "#646464", "#dedede", "#111", "NOTICE / UPDATE", "通知 / 更新", "▦"),
    "editorial-news": ("Daily Ledger", "每日纪事", "Editorial news", "新闻报道", "The story. The context. The facts.", "事件、背景与事实", "#faf8f2", "#272723", "#67675f", "#d6d3c8", "#8e3028", "THE NEWS DESK", "新闻编辑室", "DL"),
    "financial-daily": ("Market Journal", "市场日报", "Financial report", "财经报道", "Markets / Policy / Business", "市场 / 政策 / 商业", "#f7e9dc", "#142c3e", "#59646d", "#d5c6b9", "#235b73", "MARKETS & ECONOMY", "市场与经济", "MJ"),
    "technology-report": ("Signal Review", "信号观察", "Technology report", "科技报告", "Systems, ideas and what comes next", "系统、观点与未来趋势", "#11152c", "#f0f1fa", "#aaaeca", "#333955", "#a8b6ff", "FIELD NOTES / TECHNOLOGY", "现场笔记 / 科技", "↗"),
}

# Library metadata is Chinese; page defaults and crawler metadata stay English.
LIBRARY_METADATA = {
    'cybersecurity-report': ('网络安全报告模板', '研究报告', '适用于威胁情报、安全事件分析和技术研究。深色背景与薄荷绿强调，配备报告目录、中英双语字段和社交分享元信息。'),
    'fund-research': ('基金研报模板', '金融研究', '适用于基金分析、资产配置和投资研究。米白与森林绿配色、衬线标题及机构研报版式，支持中英双语和社交分享。'),
    'venture-research': ('风投研报模板', '创投研究', '适用于创业公司、行业趋势和风险投资研究。暖色纸感与深紫报头，突出观点和引用，支持双语内容及分享预览。'),
    'binance-announcement-style': ('币安公告风格模板（非官方）', '公告通知', '参考黑黄交易所公告视觉风格的独立模板，适用于产品更新及市场通知。非币安官方发布，不使用官方标识；支持双语和分享预览。'),
    'okx-announcement-style': ('OKX公告风格模板（非官方）', '公告通知', '参考黑白高对比交易所公告视觉风格的独立模板。非OKX官方发布，不使用官方标识；支持移动端、双语内容和社交分享。'),
    'editorial-news': ('新闻媒体报道模板', '新闻报道', '适用于综合新闻和深度报道。报纸式报头、衬线正文与首字下沉，支持自动目录、中英双语内容和分享卡片元信息。'),
    'financial-daily': ('财经日报模板', '财经报道', '适用于市场、经济政策与商业报道。浅鲑色纸面及海军蓝报头，采用财经编辑版式，支持双语切换和社交预览。'),
    'technology-report': ('科技报告模板', '科技研究', '适用于科技趋势、基础设施和产品研究。午夜蓝背景与现代大标题，支持响应式阅读、双语字段和分享元信息。'),
}

BASE_CSS = Path(__file__).with_name('report-styles.css').read_text()
EXTRA_CSS = {}

HTML = r'''<!doctype html>
<!-- LightCMS reusable layout. All metadata is rendered before JavaScript.
     Production gate PROD-001: set real HTTPS public URL, share image and icon fields. -->
<html lang="en" prefix="og: https://ogp.me/ns# article: https://ogp.me/ns/article#">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="theme-color" content="@@BG@@">
<title>{{.headline}} | {{if .publisher_name}}{{.publisher_name}}{{else}}@@BRAND@@{{end}}</title>
<meta name="description" content="{{.summary}}">
<meta name="author" content="{{.author}}">
<script type="application/ld+json">{"@context":"https://schema.org","@type":"Article","headline":{{.headline}},"description":{{.summary}},"datePublished":{{.published_at}},"url":{{.public_url}},"author":{"@type":"Person","name":{{.author}}}}</script>
{{if .public_url}}<link rel="canonical" href="{{.public_url}}"><meta property="og:url" content="{{.public_url}}">{{end}}
<meta property="og:type" content="article">
<meta property="og:locale" content="en_US">
<meta property="og:site_name" content="{{if .publisher_name}}{{.publisher_name}}{{else}}@@BRAND@@{{end}}">
<meta property="og:title" content="{{.headline}}">
<meta property="og:description" content="{{.summary}}">
<meta property="article:section" content="{{.category}}">
{{if .published_at}}<meta property="article:published_time" content="{{.published_at}}">{{end}}
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="{{.headline}}">
<meta name="twitter:description" content="{{.summary}}">
{{if .share_image_url}}<meta property="og:image" content="{{.share_image_url}}">
<meta property="og:image:secure_url" content="{{.share_image_url}}">
<meta property="og:image:width" content="1200"><meta property="og:image:height" content="630">
<meta property="og:image:alt" content="{{if .share_image_alt}}{{.share_image_alt}}{{else}}{{.headline}}{{end}}">
<meta name="twitter:image" content="{{.share_image_url}}">
<meta name="twitter:image:alt" content="{{if .share_image_alt}}{{.share_image_alt}}{{else}}{{.headline}}{{end}}">{{end}}
<link rel="icon" href="{{if .favicon_url}}{{.favicon_url}}{{else}}/static/images/report-templates/@@SLUG@@-icon.svg{{end}}">
<link rel="apple-touch-icon" sizes="180x180" href="{{if .touch_icon_url}}{{.touch_icon_url}}{{else}}/static/images/report-templates/@@SLUG@@-touch.png{{end}}">
<style>@@CSS@@</style>
<noscript><style>.actions,.status{display:none}</style></noscript>
</head>
<body>
<div class="loader" role="status" aria-live="polite"><div class="loader-inner"><span class="loader-mark" aria-hidden="true">@@MARK@@</span><span class="loader-label" data-i18n="loading">Loading report</span><span class="loader-track" aria-hidden="true"></span></div></div>
<div class="page lc-report" data-report-template="@@SLUG@@">
<a href="#article-content" class="skip" data-i18n="skip">Skip to article</a><div class="progress" id="progress" aria-hidden="true"></div>
<header class="masthead"><div class="wrap masthead-inner"><div class="brand"><span class="mark" aria-hidden="true">@@MARK@@</span><span id="publisher">{{if .publisher_name}}{{.publisher_name}}{{else}}@@BRAND@@{{end}}</span></div><span class="masthead-note" data-i18n="masthead">@@NOTE@@</span></div></header>
<main><article class="wrap"><div class="eyebrow"><span data-i18n="edition">@@EDITION@@</span><span data-i18n="format">@@FORMAT@@</span></div>
<header class="hero"><span class="category" id="category">{{.category}}</span><h1 id="headline">{{.headline}}</h1><p class="dek" id="summary">{{.summary}}</p><div class="meta"><span><span data-i18n="by">By</span> <strong>{{.author}}</strong></span><span><span data-i18n="published">Published</span> <time id="published-time" datetime="{{.published_at}}">{{.published_at}}</time></span><span id="reading-time"></span></div></header>
<div class="reading-layout"><div class="article-main"><div class="story" id="article-content">{{.body}}</div><div class="actions"><button class="button" id="copy-link" type="button" data-i18n="copy">Copy article link</button><button class="button" id="back-top" type="button" data-i18n="top">Back to top</button></div><span class="status" id="copy-status" role="status" aria-live="polite"></span></div>
<aside class="sidebar"><nav class="toc" id="toc" aria-labelledby="toc-title" hidden><h2 id="toc-title" data-i18n="contents">In this report</h2><ol id="toc-list"></ol></nav><div class="side-note"><h2 data-i18n="note_title">Reader's note</h2><p data-i18n="reader_note">@@READER_NOTE@@</p><p data-i18n="brand_note">@@BRAND_NOTE@@</p></div></aside></div>
</article></main><footer class="footer"><div class="wrap footer-inner"><p><span id="footer-publisher">{{if .publisher_name}}{{.publisher_name}}{{else}}@@BRAND@@{{end}}</span> · <span data-i18n="format">@@FORMAT@@</span></p><p data-i18n="footer_note">For information only. Check original sources.</p></div></footer>
</div>
{{if .headline_zh}}<template id="zh-headline">{{.headline_zh}}</template>{{end}}
{{if .summary_zh}}<template id="zh-summary">{{.summary_zh}}</template>{{end}}
{{if .category_zh}}<template id="zh-category">{{.category_zh}}</template>{{end}}
{{if .body_zh}}<template id="zh-body">{{.body_zh}}</template>{{end}}
{{if .publisher_name_zh}}<template id="zh-publisher">{{.publisher_name_zh}}</template>{{end}}
{{if .publisher_name}}<template id="custom-publisher">{{.publisher_name}}</template>{{end}}
<script>
 (function () {
 'use strict';
 /* English default for overseas readers: Chinese article data and UI
    strings stay stored but are never auto-selected. (Add an explicit UI
    toggle if manual language switching is ever needed.) */
 var isChinese = false;
 var messages = @@MESSAGES@@;
 var t = isChinese ? messages.zh : messages.en;
 document.documentElement.lang = isChinese ? 'zh-CN' : 'en';
 document.querySelectorAll('[data-i18n]').forEach(function (node) { var key = node.getAttribute('data-i18n'); if (Object.prototype.hasOwnProperty.call(t,key)) node.textContent = t[key]; });
 var story = document.getElementById('article-content');
 if (isChinese) {
   ['headline','summary','category'].forEach(function (key) { var source = document.getElementById('zh-' + key); var target = document.getElementById(key); if (source && source.content.textContent.trim()) target.textContent = source.content.textContent.trim(); else target.setAttribute('lang','en'); });
   var body = document.getElementById('zh-body');
   if (body && body.content.textContent.trim()) story.replaceChildren(body.content.cloneNode(true));
   else story.setAttribute('lang','en');
   var custom = document.getElementById('custom-publisher');
   var chinesePublisher = document.getElementById('zh-publisher');
   var name = chinesePublisher && chinesePublisher.content.textContent.trim();
   if (!name && !custom) name = t.publisher;
   if (name) { document.getElementById('publisher').textContent = name; document.getElementById('footer-publisher').textContent = name; }
 }
 document.title = document.getElementById('headline').textContent + ' | ' + document.getElementById('publisher').textContent;
 var date = document.getElementById('published-time');
 if (date.getAttribute('datetime')) { var parsed = new Date(date.getAttribute('datetime')); if (!Number.isNaN(parsed.getTime())) { date.textContent = new Intl.DateTimeFormat(isChinese ? 'zh-CN' : 'en', {year:'numeric',month:'short',day:'numeric',hour:'2-digit',minute:'2-digit',timeZone:'UTC'}).format(parsed) + ' UTC'; } }
 var text = story.textContent.trim();
 var cjk = (text.match(/[\u3400-\u9fff]/g) || []).length;
 var words = (text.replace(/[\u3400-\u9fff]/g,' ').match(/\S+/g) || []).length;
 var minutes = Math.max(1,Math.ceil(cjk / 400 + words / 220));
 document.getElementById('reading-time').textContent = t.reading.replace('{minutes}',String(minutes));
 // Build from the selected article. Preserve valid existing fragment anchors;
 // assign fresh IDs only to missing or duplicate anchors.
 var headings = Array.from(story.querySelectorAll('h2,h3'));
 var list = document.getElementById('toc-list');
 headings.forEach(function (heading,index) { var id = heading.id; var duplicates = id ? Array.from(document.querySelectorAll('[id]')).filter(function(node) { return node.id === id; }).length : 0; if (!id || duplicates !== 1) { id = 'report-section-' + index; while(document.getElementById(id)) id += '-a'; heading.id = id; } var item = document.createElement('li'); var link = document.createElement('a'); link.href = '#' + encodeURIComponent(id); link.textContent = heading.textContent.trim().replace(/^\d+[.)]\s+/, ''); item.appendChild(link); list.appendChild(item); });
 document.getElementById('toc').hidden = headings.length === 0;
 document.getElementById('copy-link').addEventListener('click',async function () { var status = document.getElementById('copy-status'); try { await navigator.clipboard.writeText(location.href); status.textContent = t.copied; } catch(error) { status.textContent = t.copy_failed; } });
 document.getElementById('back-top').addEventListener('click',function () { window.scrollTo({top:0,behavior:window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth'}); });
 function progress() { var max = document.documentElement.scrollHeight - window.innerHeight; document.getElementById('progress').style.width = (max > 0 ? Math.min(100,Math.max(0,window.scrollY / max * 100)) : 100) + '%'; }
 window.addEventListener('scroll',progress,{passive:true}); window.addEventListener('resize',progress); progress();
}());
</script>
<script>
/* In-app browser notice (iOS): webviews inside messaging/social apps cannot
   be forced into Safari, so visitors arriving from those apps get a one-tap
   guide overlay instead. iOS-only; standalone browsers are left alone. */
(function () {
 'use strict';
 var ua = navigator.userAgent || '';
 var isIOS = /iPhone|iPad|iPod/i.test(ua) && !window.MSStream;
 if (!isIOS) return;
 if (/CriOS|FxiOS|EdgiOS|OPiOS/i.test(ua)) return;
 /* Out of scope for the overseas edition: domestic apps. NOTE: this must
    precede the Messenger check — "MicroMessenger" contains "Messenger". */
 if (/MicroMessenger|WeChat|Weibo|\sQQ\//i.test(ua)) return;
 var app = '';
 if (/Telegram/i.test(ua)) app = 'Telegram';
 else if (/Instagram/i.test(ua)) app = 'Instagram';
 else if (/Messenger/i.test(ua)) app = 'Messenger';
 else if (/FBAN|FBIOS/i.test(ua)) app = 'Facebook';
 else if (/Twitter/i.test(ua)) app = 'X (Twitter)';
 else if (/LinkedInApp/i.test(ua)) app = 'LinkedIn';
 else if (/(^| )Line\//i.test(ua)) app = 'LINE';
 else if (/Snapchat/i.test(ua)) app = 'Snapchat';
 else if (/Reddit/i.test(ua)) app = 'Reddit';
 else if (/Pinterest/i.test(ua)) app = 'Pinterest';
 else if (/TikTok|musical_ly/i.test(ua)) app = 'TikTok';
 else if (/WhatsApp/i.test(ua)) app = 'WhatsApp';
 if (!app) return;
 var tip = document.createElement('div');
 tip.setAttribute('role', 'dialog');
 tip.setAttribute('aria-label', 'Open in Safari');
 tip.style.cssText = 'position:fixed;inset:0;z-index:99999;background:rgba(8,10,16,.88);color:#fff;padding:84px 28px 28px;text-align:center;font:400 17px/2 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;';
 tip.innerHTML = '<div style="font-size:15px;letter-spacing:.14em;opacity:.75">VIEWING INSIDE ' + app.toUpperCase() + '</div>' +
  '<p style="margin:22px 0 26px">For the best experience,<br>tap <b>&#8942;</b> (or Share) and choose<br><b>&ldquo;Open in Safari&rdquo;</b>.</p>' +
  '<button type="button" style="font:600 16px/1 -apple-system,BlinkMacSystemFont,sans-serif;padding:13px 46px;border-radius:10px;border:0;background:#fff;color:#111;cursor:pointer">Got it</button>';
 function dismiss() { if (tip.parentNode) tip.parentNode.removeChild(tip); }
 tip.querySelector('button').addEventListener('click', dismiss);
 function mount() { if (document.body && !tip.parentNode) document.body.appendChild(tip); }
 if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', mount);
 else mount();
}());
</script>
</body>
</html>
'''

def fields():
    result = []
    for name, label, kind in [('headline','英文标题','text'),('summary','英文摘要','textarea'),('body','英文正文（Markdown）','markdown'),('author','作者','text'),('category','文章分类','text')]:
        result.append(dict(name=name,label=label,type=kind,required=True))
    for name,label,kind in [('headline_zh','中文标题','text'),('summary_zh','中文摘要','textarea'),('body_zh','中文正文（Markdown）','markdown'),('category_zh','中文文章分类','text'),('publisher_name','发布者名称','text'),('publisher_name_zh','中文发布者名称','text'),('share_image_url','社交预览图地址（1200×630）','url'),('share_image_alt','社交预览图说明','text'),('favicon_url','网站图标地址','url'),('touch_icon_url','触屏图标地址','url')]:
        field = dict(name=name,label=label,type=kind,required=False)
        if kind == 'url': field['validation'] = dict(allowed_protocols=['https'])
        result.append(field)
    return result

def generate(slug):
    brand,brand_zh,fmt,fmt_zh,note,note_zh,bg,ink,muted,line,accent,edition,edition_zh,mark = THEMES[slug]
    palettes = {
        'cybersecurity-report': ('#0c1a1d','#e6f4f1','#afc4bf','#24464a','#68ceb1'),
        'fund-research': ('#f8f9f6','#183b33','#566c61','#dbe4dc','#29634c'),
        'venture-research': ('#f7f4f1','#352843','#72677b','#e0d9e3','#95513e'),
        'binance-announcement-style': ('#fbfcfd','#232b32','#65717d','#e1e6ea','#8f6800'),
        'okx-announcement-style': ('#fafbfa','#212424','#666b6b','#dde1e0','#242828'),
        'editorial-news': ('#faf8f3','#252d30','#657071','#deded5','#8b423b'),
        'financial-daily': ('#f5e8db','#243845','#626e75','#ddcbbc','#2c6275'),
        'technology-report': ('#141c2c','#edf2f7','#a8b7cc','#34465f','#9ebade'),
    }
    bg,ink,muted,line,accent = palettes[slug]
    icons = {
        'cybersecurity-report':'<path d="M16 3 27 7v8c0 8-11 14-11 14S5 23 5 15V7z"/><path d="m10 16 4 4 8-9"/>',
        'fund-research':'<rect x="4" y="4" width="24" height="24" rx="3"/><path d="M10 23V9l6 8 6-8v14"/>',
        'venture-research':'<path d="M7 26V6l18 20V6"/>',
        'binance-announcement-style':'<path d="m16 3 13 13-13 13L3 16z"/><path d="M11 16h10m-5-5v10"/>',
        'okx-announcement-style':'<rect x="5" y="4" width="22" height="24" rx="2"/><path d="M11 11h10m-10 5h10m-10 5h6"/>',
        'editorial-news':'<rect x="4" y="5" width="24" height="22" rx="2"/><path d="M9 10h14M9 16h5m4 0h5M9 21h5m4 0h5"/>',
        'financial-daily':'<path d="M5 26h23M9 21v-6m7 6V9m7 12V4"/>',
        'technology-report':'<path d="M5 22h5l5-13 6 17 6-17"/>',
    }
    mark = '<svg viewBox="0 0 32 32" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">'+icons[slug]+'</svg>'
    edition = {'cybersecurity-report':'Security intelligence','fund-research':'Investment research','venture-research':'Venture insights','binance-announcement-style':'Announcements','okx-announcement-style':'Product notices','editorial-news':'The news desk','financial-daily':'Markets and economy','technology-report':'Technology field notes'}[slug]
    exchange = slug in ('binance-announcement-style','okx-announcement-style')
    financial = slug in ('fund-research','venture-research','financial-daily')
    reader = 'Read the scope, supporting evidence and limitations before drawing conclusions.'
    reader_zh = '形成结论前，请阅读分析范围、支持证据与局限。'
    if financial:
        reader += ' This report is not investment advice.'
        reader_zh += ' 本报告不构成投资建议。'
    brand_note = 'An independent publishing layout. Publisher identity is supplied by the content owner.'
    brand_note_zh = '独立出版版式；发布者身份由内容所有者提供。'
    if exchange:
        brand_note = 'Independent announcement layout. Not an official Binance or OKX publication.'
        brand_note_zh = '独立公告版式，并非币安或 OKX 官方发布。'
    en = dict(loading='Loading report',skip='Skip to article',masthead=note,edition=edition,format=fmt,by='By',published='Published',copy='Copy article link',top='Back to top',contents='In this report',note_title="Reader's note",reader_note=reader,brand_note=brand_note,footer_note='For information only. Check original sources.',publisher=brand,reading='{minutes} min read',copied='Link copied',copy_failed='Could not copy. Use the address bar.')
    zh = dict(loading='正在加载报告',skip='跳转到正文',masthead=note_zh,edition=edition_zh,format=fmt_zh,by='作者',published='发布于',copy='复制文章链接',top='返回顶部',contents='报告目录',note_title='阅读提示',reader_note=reader_zh,brand_note=brand_note_zh,footer_note='内容仅供参考，请核对原始来源。',publisher=brand_zh,reading='阅读约 {minutes} 分钟',copied='链接已复制',copy_failed='复制失败，请使用地址栏。')
    values = dict(SLUG=slug,BRAND=brand,FORMAT=fmt,NOTE=note,BG=bg,INK=ink,MUTED=muted,LINE=line,ACCENT=accent,SCHEME='dark' if slug in ('cybersecurity-report','technology-report') else 'light',EDITION=edition,MARK=mark,READER_NOTE=reader,BRAND_NOTE=brand_note,MESSAGES=json.dumps(dict(en=en,zh=zh),ensure_ascii=False))
    css = BASE_CSS + EXTRA_CSS.get(slug, '')
    for key,value in values.items(): css = css.replace('@@'+key+'@@',value)
    values['CSS'] = css
    html = HTML
    for key,value in values.items(): html = html.replace('@@'+key+'@@',value)
    html = html.rstrip('\n') + '\n'
    # POST /api/v1/templates accepts these fields; new templates default active.
    # ScriptPolicy is inherited, not a writable field on this legacy transport.
    library_name,library_category,library_description = LIBRARY_METADATA[slug]
    template = dict(slug=slug,name=library_name,category=library_category,description=library_description,fields=fields(),html_layout=html)
    data = dict(headline='A clearer view of the next market cycle',summary='An illustrative report showing how evidence, context and limitations can be presented in a readable publishing format.',author='Research desk',category=fmt,headline_zh='更清晰地观察下一个市场周期',summary_zh='示例报告展示如何清晰呈现证据、背景与分析局限。',category_zh=fmt_zh,body='## Executive summary\n\nThis is **illustrative template content**, not a real market forecast, security finding or exchange announcement. Replace every paragraph with verified reporting before publication.\n\n## Evidence and context\n\nExplain the source, observation date and measurement method. Separate documented facts from interpretation.\n\n| Observation | Source | Limitation |\n| --- | --- | --- |\n| Insert verified observation | Link to original source | Explain coverage |\n| Add comparable evidence | Describe methodology | State uncertainty |\n\n> A strong conclusion should make its assumptions visible.\n\n## What to watch next\n\n1. Identify a measurable follow-up signal.\n2. Explain what would challenge the thesis.\n3. State the relevant timeframe.\n\n## Sources and limitations\n\nAdd source links, dates and limitations here. This demonstration contains no investment recommendation.',body_zh='## 核心摘要\n\n这是**模板演示内容**，并非真实市场预测、安全发现或交易所公告。发布前请使用经核实的报道替换所有段落。\n\n## 证据与背景\n\n注明信息来源、观察日期与测量方法，区分事实和解读。\n\n| 观察 | 来源 | 局限 |\n| --- | --- | --- |\n| 填入经核实的观察 | 原始来源链接 | 说明覆盖范围 |\n\n> 有说服力的结论应让假设清晰可见。\n\n## 后续关注\n\n1. 定义可测量的后续信号。\n2. 说明哪些变化会挑战判断。\n3. 明确适用时间范围。\n\n## 来源与局限\n\n在这里补充来源、日期和局限。本示例不提供投资建议。',share_image_url='https://publisher.example/static/images/report-templates/'+slug+'-share.png',share_image_alt=fmt+' — illustrative report cover',favicon_url='https://publisher.example/static/images/report-templates/'+slug+'-icon.svg',touch_icon_url='https://publisher.example/static/images/report-templates/'+slug+'-touch.png')
    example = dict(template=slug,title=data['headline'],slug=slug+'-demo',folder_path='/reports',mode='draft',data=data)
    return {slug+'.html':html,slug+'.template.json':json.dumps(template,ensure_ascii=False,indent=2)+'\n',slug+'.example.json':json.dumps(example,ensure_ascii=False,indent=2)+'\n'}

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('slug',choices=list(THEMES))
    parser.add_argument('--check',action='store_true')
    args = parser.parse_args()
    outputs = generate(args.slug)
    if args.check:
        for name,content in outputs.items():
            assert (Path(__file__).parent / name).read_text() == content, name+' is out of date'
        return
    print('*** Begin Patch')
    for name,content in outputs.items():
        path = Path(__file__).parent / name
        if path.exists():
            print('*** Update File: '+str(path))
            print('@@')
            for line in path.read_text().splitlines(): print('-'+line)
        else:
            print('*** Add File: '+str(path))
        for line in content.splitlines(): print('+'+line)
    print('*** End Patch')

if __name__ == '__main__': main()
