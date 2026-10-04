"""Emit import/example JSON for the existing crypto-analysis HTML layout."""
import importlib.util
import json
import sys
from pathlib import Path

sys.dont_write_bytecode = True
HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('builder', HERE/'build-report-templates.py')
builder = importlib.util.module_from_spec(spec)
spec.loader.exec_module(builder)
fields = [f for f in builder.fields() if f['name'] in {'headline','summary','body','author','category','share_image_url','share_image_alt','favicon_url'}]
for f in fields:
    if f['name'] == 'body':
        f.update(type='richtext',label='正文（富文本）')
    elif f['name'] == 'headline': f['label'] = '标题'
    elif f['name'] == 'summary': f['label'] = '摘要'
payload = dict(slug='crypto-analysis',name='加密货币分析模板',category='加密研究',description='适用于加密货币市场、行情结构与链上数据分析。内嵌响应式样式、500毫秒加载效果、中英界面及社交分享元信息；正文采用富文本格式。',fields=fields,html_layout=(HERE/'crypto-analysis.html').read_text())
example = dict(template='crypto-analysis',title='Digital asset market review',slug='crypto-analysis-demo',folder_path='/reports',mode='draft',data=dict(headline='Digital asset market review',summary='Illustrative template content. Replace with verified reporting.',body='<h2>Evidence and context</h2><p>This is an illustrative report, not an investment recommendation.</p>',author='Research desk',category='Digital assets',share_image_url='https://publisher.example/static/images/chain-lens-share.png',share_image_alt='Digital asset market analysis',favicon_url='https://publisher.example/static/images/chain-lens-icon.svg'))
print('*** Begin Patch')
for name,value in [('crypto-analysis.template.json',payload),('crypto-analysis.example.json',example)]:
    target=HERE/name
    if target.exists():
        print('*** Update File: '+str(target));print('@@')
        for line in target.read_text().splitlines(): print('-'+line)
    else: print('*** Add File: '+str(target))
    for line in (json.dumps(value,ensure_ascii=False,indent=2)+'\n').splitlines(): print('+'+line)
print('*** End Patch')
