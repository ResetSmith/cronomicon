"""Generate the Cronomicon brand asset set from the text-free emblem.

Input:  assets/cronomicon-emblem.png (1024px, transparent, no lettering).
Fonts:  IBM Plex TTFs (Sans Medium + SemiBold, Sans Condensed Bold) committed under
        assets/fonts/ (OFL, licence alongside; upstream https://github.com/IBM/plex).
        $PLEX_TTF_DIR overrides the directory.
Output: the lockups and social preview in assets/, the in-app emblem, the favicon set, and
        manual-logo-b64.txt to paste into the manuals' <div class="logo"> data URI.
Re-run after changing the emblem; every output is derived. Requires Pillow."""
from PIL import Image, ImageDraw, ImageFont
import base64, io, os, sys
S = os.path.dirname(os.path.abspath(__file__))  # assets/
ROOT = os.path.dirname(S)
FONTS = os.environ.get("PLEX_TTF_DIR", S + "/fonts")
EMBLEM = S + "/cronomicon-emblem.png"
NAME, TAGLINE = "CRONOMICON", "ANCIENT RITES OF SCHEDULING, MADE EASY"
NAVY, LIGHT_TEXT, GOLD, DARK_BG, MUTED_DARK, MUTED_LIGHT = "#131e2b", "#e8eff7", "#c9a227", "#0a1119", "#9db2c8", "#46596e"

def font(name, px): return ImageFont.truetype(f"{FONTS}/{name}.ttf", px)
def emblem(size):
    im = Image.open(EMBLEM).convert("RGBA")
    im = im.crop(im.getbbox())                       # trim transparent margin
    im.thumbnail((size, size), Image.LANCZOS)
    canvas = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    canvas.paste(im, ((size - im.width) // 2, (size - im.height) // 2), im)
    return canvas
def text_w(draw, s, f, spacing):
    return sum(draw.textlength(ch, font=f) for ch in s) + spacing * (len(s) - 1)
def draw_spaced(draw, xy, s, f, fill, spacing):
    x, y = xy
    for ch in s:
        draw.text((x, y), ch, font=f, fill=fill); x += draw.textlength(ch, font=f) + spacing

def stacked(width, name_color, tag_color, bg=None):
    em = emblem(int(width * 0.56))
    fn, ft = font("IBMPlexSansCondensed-Bold", int(width * 0.135)), font("IBMPlexSans-SemiBold", int(width * 0.037))
    sp_n, sp_t = width * 0.012, width * 0.004
    probe = ImageDraw.Draw(Image.new("RGBA", (1, 1)))
    nw, tw = text_w(probe, NAME, fn, sp_n), text_w(probe, TAGLINE, ft, sp_t)
    gap1, gap2, pad = int(width * 0.04), int(width * 0.02), int(width * 0.05)
    nh, th = int(fn.size * 1.0), int(ft.size * 1.2)
    H = pad + em.height + gap1 + nh + gap2 + th + pad
    im = Image.new("RGBA", (width, H), bg if bg else (0, 0, 0, 0))
    im.paste(em, ((width - em.width) // 2, pad), em)
    d = ImageDraw.Draw(im)
    y = pad + em.height + gap1
    draw_spaced(d, ((width - nw) / 2, y - fn.size * 0.15), NAME, fn, name_color, sp_n)
    draw_spaced(d, ((width - tw) / 2, y + nh + gap2), TAGLINE, ft, tag_color, sp_t)
    return im

def horizontal(height, name_color, tag_color, bg=None, tagline=True):
    em = emblem(height)
    fn = font("IBMPlexSansCondensed-Bold", int(height * 0.46)); ft = font("IBMPlexSans-SemiBold", int(height * 0.13))
    sp_n, sp_t = height * 0.03, height * 0.012
    probe = ImageDraw.Draw(Image.new("RGBA", (1, 1)))
    nw = text_w(probe, NAME, fn, sp_n); tw = text_w(probe, TAGLINE, ft, sp_t) if tagline else 0
    gap = int(height * 0.18); pad = int(height * 0.08)
    W = pad + em.width + gap + int(max(nw, tw)) + pad
    im = Image.new("RGBA", (W, height + 2 * pad), bg if bg else (0, 0, 0, 0))
    im.paste(em, (pad, pad), em); d = ImageDraw.Draw(im)
    x = pad + em.width + gap
    block = fn.size + (int(ft.size * 1.6) if tagline else 0)
    y = pad + (height - block) // 2
    draw_spaced(d, (x, y - fn.size * 0.15), NAME, fn, name_color, sp_n)
    if tagline: draw_spaced(d, (x, y + fn.size + int(ft.size * 0.35)), TAGLINE, ft, tag_color, sp_t)
    return im

def social():
    W, H = 1280, 640
    im = Image.new("RGBA", (W, H), DARK_BG)
    em = emblem(440); im.paste(em, (120, (H - em.height) // 2), em)
    d = ImageDraw.Draw(im)
    x = 120 + em.width + 64; avail = W - x - 72
    fn = font("IBMPlexSansCondensed-Bold", 132)
    while text_w(d, NAME, fn, 4) > avail: fn = font("IBMPlexSansCondensed-Bold", fn.size - 2)
    ft = font("IBMPlexSans-SemiBold", 22)
    while text_w(d, TAGLINE, ft, 1) > avail: ft = font("IBMPlexSans-SemiBold", ft.size - 1)
    fs = font("IBMPlexSans-Medium", 24)
    y = (H - (fn.size + 24 + ft.size + 44 + 2 * 34)) // 2
    draw_spaced(d, (x, y - fn.size * 0.15), NAME, fn, GOLD, 4)
    draw_spaced(d, (x + 3, y + fn.size + 24), TAGLINE, ft, LIGHT_TEXT, 1)
    d.text((x + 3, y + fn.size + 24 + ft.size + 44), "Schedule and orchestrate Bash, Ansible, Terraform,", font=fs, fill=MUTED_DARK)
    d.text((x + 3, y + fn.size + 24 + ft.size + 44 + 34), "PowerShell, Perl and Python jobs from one binary.", font=fs, fill=MUTED_DARK)
    return im

out_assets, out_pub, out_src = S, ROOT + "/frontend/public", ROOT + "/frontend/src/assets"
os.makedirs(out_assets, exist_ok=True)
stacked(1024, NAVY, MUTED_LIGHT).save(f"{out_assets}/cronomicon-logo.png")
stacked(1024, GOLD, MUTED_DARK).save(f"{out_assets}/cronomicon-logo-dark.png")
horizontal(256, NAVY, MUTED_LIGHT).save(f"{out_assets}/cronomicon-lockup.png")
horizontal(256, GOLD, MUTED_DARK).save(f"{out_assets}/cronomicon-lockup-dark.png")
social().save(f"{out_assets}/cronomicon-social.png")
# app assets
emblem(336).save(f"{out_src}/logo-emblem.png", optimize=True)
emblem(256).save(f"{out_pub}/cronomicon-icon.png")
ico = emblem(256); ico.save(f"{out_pub}/favicon.ico", sizes=[(16,16),(32,32),(48,48),(64,64),(128,128),(256,256)])
# manuals: 128px data URI
buf = io.BytesIO(); emblem(128).save(buf, "PNG", optimize=True)
open(S + "/manual-logo-b64.txt", "w").write(base64.b64encode(buf.getvalue()).decode())
print("done")
