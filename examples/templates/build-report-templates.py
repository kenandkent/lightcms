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

BASE_CSS = r'''
:root{color-scheme:@@SCHEME@@;--page:@@BG@@;--ink:@@INK@@;--muted:@@MUTED@@;--line:@@LINE@@;--accent:@@ACCENT@@;--delay:500ms}
*,:before,:after{box-sizing:border-box}html{scroll-behavior:smooth}body{margin:0;background:var(--page);color:var(--ink);font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;-webkit-font-smoothing:antialiased}a{color:inherit}button{font:inherit}button,a{-webkit-tap-highlight-color:transparent}:focus-visible{outline:2px solid var(--accent);outline-offset:5px}
.page{visibility:hidden;animation:show-page 0s linear var(--delay) forwards}.loader{position:fixed;inset:0;z-index:100;background:var(--page);display:grid;place-items:center;animation:hide-loader 0s linear var(--delay) forwards}.loader-inner{display:grid;gap:20px;text-align:center}.loader-mark{font-size:42px;color:var(--accent)}.loader-label{font-size:13px;letter-spacing:.08em;color:var(--muted)}.loader-track{height:2px;width:120px;background:var(--line);overflow:hidden;margin:auto}.loader-track:after{content:"";display:block;height:100%;width:40%;background:var(--accent);animation:sweep .5s ease-in-out infinite alternate}@keyframes show-page{to{visibility:visible}}@keyframes hide-loader{to{visibility:hidden}}@keyframes sweep{to{transform:translateX(150%)}}
.wrap{width:min(1160px,calc(100% - 80px));margin-inline:auto}.masthead{border-bottom:1px solid var(--line)}.masthead-inner{min-height:90px;display:flex;justify-content:space-between;align-items:center;gap:24px}.brand{display:flex;align-items:center;gap:14px;font-size:21px;font-weight:750;letter-spacing:-.03em}.mark{font-size:28px;color:var(--accent)}.masthead-note{font-size:12px;color:var(--muted)}.eyebrow{display:flex;justify-content:space-between;gap:16px;font-size:11px;letter-spacing:.12em;font-weight:650;color:var(--accent);padding:24px 0;border-bottom:1px solid var(--line)}.hero{padding:64px 0 48px}.category{font-size:12px;font-weight:650;letter-spacing:.12em;text-transform:uppercase;color:var(--accent);display:block;margin-bottom:22px}h1{font-size:clamp(36px,5.3vw,70px);letter-spacing:-.045em;line-height:1.08;max-width:22ch;margin:0;overflow-wrap:anywhere}.dek{font-size:clamp(18px,1.8vw,22px);line-height:1.65;color:var(--muted);max-width:65ch;margin:26px 0}.meta{display:flex;gap:12px 28px;flex-wrap:wrap;color:var(--muted);font-size:12px;line-height:1.8;font-variant-numeric:tabular-nums}.meta strong{color:var(--ink);font-weight:600}.reading-layout{display:grid;grid-template-columns:minmax(0,1fr) 240px;gap:80px;border-top:1px solid var(--line);padding:48px 0 80px}.story{min-width:0;max-width:72ch;font-size:17px;line-height:1.9;overflow-wrap:anywhere}.story>:first-child{margin-top:0}.story h2{font-size:clamp(24px,2.6vw,32px);line-height:1.3;letter-spacing:-.03em;margin:2.1em 0 .7em}.story h3{font-size:22px;line-height:1.4;margin:1.8em 0 .6em}.story p,.story ul,.story ol{margin:0 0 1.25em}.story a{color:var(--accent);text-underline-offset:4px}.story blockquote{margin:28px 0;padding:6px 0 6px 24px;border-left:3px solid var(--accent);font-size:1.12em}.story img,.story video{max-width:100%;height:auto}.story pre{overflow:auto;background:var(--line);padding:20px;font-size:13px;line-height:1.65}.story table{display:block;max-width:100%;overflow:auto;border-collapse:collapse;font-size:14px;margin:30px 0}.story th,.story td{padding:13px 20px 13px 0;text-align:left;border-bottom:1px solid var(--line);min-width:120px}.sidebar{align-self:start;position:sticky;top:24px;border-top:3px solid var(--accent);padding-top:20px;font-size:13px;line-height:1.75;color:var(--muted)}.sidebar h2{font-size:12px;color:var(--ink);margin:0 0 14px;letter-spacing:.1em;text-transform:uppercase}.toc ol{padding-left:18px;margin:0 0 28px}.toc a{display:inline-block;padding:5px 0;text-decoration:none}.toc a:hover{text-decoration:underline}.side-note{border-top:1px solid var(--line);padding-top:20px}.actions{display:flex;gap:12px;flex-wrap:wrap;margin-top:40px}.button{border:1px solid var(--line);background:transparent;color:var(--ink);padding:12px 18px;min-height:44px;cursor:pointer;font-size:12px}.button:hover{border-color:var(--accent)}.status{display:block;min-height:24px;font-size:12px;color:var(--muted);margin-top:10px}.footer{border-top:1px solid var(--line);padding:26px 0 32px}.footer-inner{display:flex;justify-content:space-between;gap:24px;font-size:11px;line-height:1.8;color:var(--muted)}.footer p{margin:0}.skip{position:absolute;left:20px;top:-80px;z-index:101;background:var(--ink);color:var(--page);padding:14px}.skip:focus{top:12px}.progress{position:fixed;left:0;top:0;height:3px;background:var(--accent);width:0;z-index:90;pointer-events:none}[hidden]{display:none!important}
@media(max-width:850px){.wrap{width:calc(100% - 40px)}.reading-layout{grid-template-columns:minmax(0,1fr);gap:40px}.sidebar{position:static}.hero{padding-top:44px}.masthead-note{display:none}.story{max-width:none}.footer-inner{flex-direction:column;gap:10px}}
@media(max-width:480px){.wrap{width:calc(100% - 32px)}.masthead-inner{min-height:70px}.brand{font-size:18px}.hero{padding:36px 0 30px}h1{font-size:36px;max-width:none}.dek{font-size:18px}.story{font-size:16px;line-height:1.85}.reading-layout{padding-top:32px}.eyebrow{font-size:10px;letter-spacing:.05em}.eyebrow>span:last-child{display:none}.meta{gap:8px 20px}}
@media(prefers-reduced-motion:reduce){html{scroll-behavior:auto}.loader-track:after{animation:none;width:100%}}
@media print{body{background:#fff;color:#111}.page{visibility:visible;animation:none}.loader,.actions,.status,.sidebar,.progress,.skip{display:none}.reading-layout{display:block}.wrap{width:100%}.dek,.meta,.footer{color:#333}.hero{padding:24px 0}h1{font-size:32px}.story{max-width:none}.story pre{background:#eee}}
'''

EXTRA_CSS = {
    "cybersecurity-report": '.masthead-inner{min-height:76px}.brand,.eyebrow,.meta,.sidebar h2{font-family:ui-monospace,SFMono-Regular,Consolas,monospace}.mark{border:1px solid var(--accent);width:40px;text-align:center}.hero{border-left:2px solid var(--accent);padding-left:32px;margin-block:32px}.reading-layout{grid-template-columns:220px minmax(0,1fr);gap:60px}.sidebar{grid-column:1;grid-row:1}.article-main{grid-column:2;grid-row:1}h1{max-width:20ch}.story h2:before{content:"// ";color:var(--accent);font-family:monospace}@media(max-width:850px){.reading-layout{grid-template-columns:minmax(0,1fr)}.sidebar,.article-main{grid-column:1;grid-row:auto}.sidebar{order:2}.hero{padding-left:20px}}',
    "fund-research": '.masthead{border-top:7px solid var(--ink)}.brand{font-family:Georgia,"Times New Roman",serif;font-size:28px}.mark{font-family:Georgia,serif;border:1px solid var(--ink);font-size:28px;width:44px;height:44px;display:grid;place-items:center}.hero{padding:72px 0 50px}h1{font-family:Georgia,"Times New Roman",serif;font-weight:400;letter-spacing:-.04em;max-width:19ch}.dek{padding-left:24px;border-left:3px solid var(--accent)}.sidebar{background:#e9eee5;padding:24px;border:0}.story h2{font-family:Georgia,"Times New Roman",serif;font-weight:500}.footer{border-top:3px double var(--ink)}',
    "venture-research": '.masthead{background:#302447;color:#f4f0eb}.masthead .mark{color:#ffb48d}.masthead-note{color:#d3c7e6}.brand{letter-spacing:-.05em}.hero{position:relative;padding:76px 0 52px}.hero:after{content:"↗";position:absolute;right:0;top:50px;font-size:150px;color:#ded5e7;z-index:-1}.category{color:#a84220}.dek{max-width:52ch}h1{max-width:17ch;font-weight:650;font-size:clamp(40px,6vw,78px)}.reading-layout{grid-template-columns:minmax(0,1fr) 280px;gap:64px}.sidebar{padding:28px;background:#e9e2ef;border:0;border-radius:2px}.story blockquote{background:#e9e2ef;padding:24px;border-left:4px solid #a84220}@media(max-width:850px){.reading-layout{grid-template-columns:minmax(0,1fr)}.hero:after{display:none}}',
    "binance-announcement-style": '.masthead{background:#181b20;color:#fff}.masthead .mark{color:#f3c746}.masthead-note{color:#b9bec7}.eyebrow{background:#fff6d5;padding:18px 24px;margin-top:28px;border:0;color:#795400}.wrap{max-width:1040px}.hero{max-width:830px;margin:auto;padding:56px 0 36px}h1{font-size:clamp(34px,4.2vw,52px);max-width:27ch;line-height:1.2;letter-spacing:-.025em}.category{background:#fbe7a5;color:#614500;display:inline-block;padding:7px 11px;font-size:11px;margin-bottom:20px}.dek{font-size:18px}.reading-layout{max-width:830px;margin:auto;grid-template-columns:minmax(0,1fr);gap:32px}.sidebar{position:static;border:1px solid var(--line);background:#fafafa;padding:24px}.story{max-width:none;font-size:16px}.button:first-child{background:#f3c746;border-color:#f3c746;color:#181b20}.footer{background:#fafafa}',
    "okx-announcement-style": '.masthead{border-bottom:3px solid #111}.brand{font-size:24px;font-weight:850;letter-spacing:-.055em}.mark{font-size:36px}.eyebrow{color:#111;font-weight:800}.hero{padding:64px 0 42px}h1{font-size:clamp(38px,5.5vw,68px);font-weight:850;max-width:21ch}.category{background:#111;color:#fff;width:fit-content;padding:9px 13px}.dek{font-size:19px;color:#555}.reading-layout{grid-template-columns:minmax(0,1fr) 220px;gap:64px;border-top:3px solid #111}.sidebar{border-top:0;padding:0}.sidebar h2{font-weight:800}.button{border:1px solid #111;font-weight:700}.button:first-child{background:#111;color:#fff}.footer{border-top:3px solid #111}@media(max-width:850px){.reading-layout{grid-template-columns:minmax(0,1fr)}}',
    "editorial-news": '.masthead-inner{justify-content:center;flex-direction:column;gap:8px;padding:28px 0 24px}.brand{font:700 clamp(28px,4vw,48px)/1.1 Georgia,"Times New Roman",serif;letter-spacing:-.055em}.mark{display:none}.masthead-note{font-family:Georgia,serif;font-style:italic}.masthead{border-bottom:4px double var(--ink)}.eyebrow{color:var(--muted)}.hero{text-align:center;max-width:930px;margin:auto;padding:52px 0 40px}h1{font-family:Georgia,"Times New Roman",serif;font-size:clamp(38px,5.2vw,64px);font-weight:500;letter-spacing:-.045em;max-width:none}.dek{font-family:Georgia,serif;margin:26px auto;max-width:60ch}.meta{justify-content:center}.story{font-family:Georgia,"Times New Roman","Songti SC",serif;font-size:19px;line-height:1.85}.story h2,.story h3{font-family:Georgia,"Times New Roman",serif}.reading-layout{grid-template-columns:minmax(0,1fr) 230px;gap:70px;border-top:4px double var(--ink)}.sidebar{border-top:0;border-left:1px solid var(--line);padding:0 0 0 24px}.story>p:first-of-type:first-letter{float:left;font-size:3.5em;line-height:.95;padding:7px 8px 0 0;color:var(--accent)}@media(max-width:850px){.reading-layout{grid-template-columns:minmax(0,1fr)}.sidebar{border-left:0;padding-left:0}.story{font-size:18px}}',
    "financial-daily": '.masthead{background:#142c3e;color:#f7e9dc;border:0}.masthead-inner{min-height:84px}.brand{font-family:Georgia,"Times New Roman",serif;font-size:30px;font-weight:500}.mark{color:#f7e9dc;font-family:Georgia,serif;font-size:22px;border-right:1px solid #647785;padding-right:16px}.masthead-note{color:#bdc9d0}.eyebrow{border-bottom:3px double var(--ink);font-family:Georgia,serif;font-size:12px;letter-spacing:.04em;color:var(--ink)}.hero{padding-top:48px}h1{font-family:Georgia,"Times New Roman",serif;font-weight:500;max-width:24ch;font-size:clamp(38px,5vw,64px)}.dek{font-family:Georgia,serif;font-size:21px;max-width:65ch}.reading-layout{border-top:3px double var(--ink);gap:64px}.story{font-family:Georgia,"Times New Roman","Songti SC",serif;font-size:18px}.sidebar{border-top:3px solid #142c3e}.footer{border-top:3px double var(--ink)}',
    "technology-report": '.masthead{border-top:5px solid #6878e8}.mark{font-size:40px;font-weight:300}.brand{font-weight:600;letter-spacing:-.06em}.hero{padding:72px 0 54px;border-bottom:1px solid var(--line)}h1{font-weight:550;font-size:clamp(40px,6.3vw,80px);max-width:18ch;letter-spacing:-.055em}.category,.eyebrow{font-family:ui-monospace,Consolas,monospace}.dek{max-width:54ch}.reading-layout{border:0;grid-template-columns:minmax(0,1fr) 260px;gap:64px}.sidebar{border:1px solid #404a78;background:#1b2244;padding:26px}.story h2{font-weight:550}.button{border-radius:24px}.button:first-child{background:#a8b6ff;color:#11152c;border:0}@media(max-width:850px){.reading-layout{grid-template-columns:minmax(0,1fr)}}',
}

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
<div class="page">
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
 var preferred = (navigator.languages && navigator.languages[0]) || navigator.language || 'en';
 var isChinese = /^zh(?:-|$)/i.test(preferred);
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
 headings.forEach(function (heading,index) { var id = heading.id; var duplicates = id ? Array.from(document.querySelectorAll('[id]')).filter(function(node) { return node.id === id; }).length : 0; if (!id || duplicates !== 1) { id = 'report-section-' + index; while(document.getElementById(id)) id += '-a'; heading.id = id; } var item = document.createElement('li'); var link = document.createElement('a'); link.href = '#' + encodeURIComponent(id); link.textContent = heading.textContent; item.appendChild(link); list.appendChild(item); });
 document.getElementById('toc').hidden = headings.length === 0;
 document.getElementById('copy-link').addEventListener('click',async function () { var status = document.getElementById('copy-status'); try { await navigator.clipboard.writeText(location.href); status.textContent = t.copied; } catch(error) { status.textContent = t.copy_failed; } });
 document.getElementById('back-top').addEventListener('click',function () { window.scrollTo({top:0,behavior:window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth'}); });
 function progress() { var max = document.documentElement.scrollHeight - window.innerHeight; document.getElementById('progress').style.width = (max > 0 ? Math.min(100,Math.max(0,window.scrollY / max * 100)) : 100) + '%'; }
 window.addEventListener('scroll',progress,{passive:true}); window.addEventListener('resize',progress); progress();
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
    css = BASE_CSS + EXTRA_CSS[slug]
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
