"""Generate original typographic social covers and touch icons (no official logos)."""
import importlib.util
import sys
from pathlib import Path
from PIL import Image, ImageDraw, ImageFont

ROOT = Path(__file__).resolve().parents[2]
sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location('report_builder',Path(__file__).with_name('build-report-templates.py'))
builder = importlib.util.module_from_spec(spec)
spec.loader.exec_module(builder)
destination = ROOT / 'static/images/report-templates'
destination.mkdir(parents=True,exist_ok=True)
regular = '/System/Library/Fonts/Supplemental/Arial.ttf'
bold = '/System/Library/Fonts/Supplemental/Arial Bold.ttf'

for slug,theme in builder.THEMES.items():
    brand,_,kind,_,note,_,bg,ink,muted,line,accent,*_ = theme
    cover = Image.new('RGB',(1200,630),bg)
    draw = ImageDraw.Draw(cover)
    draw.rectangle((0,0,1200,12),fill=accent)
    draw.rounded_rectangle((72,64,130,122),radius=8,fill=ink)
    initial = brand[0]
    draw.text((89,73),initial,font=ImageFont.truetype(bold,34),fill=bg)
    draw.text((154,74),brand,font=ImageFont.truetype(bold,32),fill=ink)
    draw.line((72,160,1128,160),fill=line,width=2)
    # Wrap long style labels to retain the 1200 x 630 safe area.
    words = kind.split(); lines=[]; current=''
    heading = ImageFont.truetype(bold,76)
    for word in words:
        candidate = (current+' '+word).strip()
        if draw.textbbox((0,0),candidate,font=heading)[2] > 1030 and current:
            lines.append(current); current=word
        else: current=candidate
    lines.append(current)
    for i,text in enumerate(lines): draw.text((72,215+i*90),text,font=heading,fill=ink)
    draw.text((76,440),note,font=ImageFont.truetype(regular,28),fill=muted)
    draw.line((72,525,1128,525),fill=line,width=2)
    draw.text((76,553),'INDEPENDENT PUBLISHING / REPORT SERIES',font=ImageFont.truetype(bold,18),fill=accent)
    cover.save(destination / (slug+'-share.png'))
    icon = Image.new('RGB',(180,180),bg)
    d = ImageDraw.Draw(icon)
    d.rounded_rectangle((10,10,170,170),radius=28,fill=ink)
    font = ImageFont.truetype(bold,96)
    box=d.textbbox((0,0),initial,font=font)
    d.text(((180-(box[2]-box[0]))/2,(180-(box[3]-box[1]))/2-box[1]),initial,font=font,fill=bg)
    icon.save(destination / (slug+'-touch.png'))
