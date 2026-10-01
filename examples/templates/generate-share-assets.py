"""Regenerate the default Chain Lens PNG assets using Pillow."""
from pathlib import Path
from PIL import Image, ImageDraw, ImageFont

root = Path(__file__).resolve().parents[2]
destination = root / "static" / "images"
font_path = "/System/Library/Fonts/Supplemental/Arial.ttf"
bold_path = "/System/Library/Fonts/Supplemental/Arial Bold.ttf"
image = Image.new("RGB", (1200, 630), "#101c29")
draw = ImageDraw.Draw(image)
draw.polygon([(100, 72), (128, 100), (100, 128), (72, 100)], outline="#7acdc0", width=4)
draw.text((152, 74), "Chain Lens", font=ImageFont.truetype(bold_path, 40), fill="#eef3f5")
draw.line((72, 178, 1128, 178), fill="#304555", width=2)
draw.text((72, 248), "Crypto Market", font=ImageFont.truetype(bold_path, 80), fill="#eef3f5")
draw.text((72, 342), "Analysis", font=ImageFont.truetype(bold_path, 80), fill="#7acdc0")
draw.text((76, 524), "Market structure. Data. Independent perspective.", font=ImageFont.truetype(font_path, 28), fill="#c8d5dc")
image.save(destination / "chain-lens-share.png", optimize=True)

icon = Image.new("RGB", (180, 180), "#101c29")
draw = ImageDraw.Draw(icon)
draw.polygon([(90, 28), (152, 90), (90, 152), (28, 90)], outline="#7acdc0", width=10)
draw.polygon([(90, 64), (116, 90), (90, 116), (64, 90)], fill="#e0bb79")
icon.save(destination / "chain-lens-touch.png", optimize=True)
