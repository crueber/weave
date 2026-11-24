#!/usr/bin/env python3
"""Turns the raw Playwright recordings from record.mjs into docs/demo.webm and docs/demo.webp.

  DEMO_DIR=... FFMPEG=/path/to/ffmpeg POSTER=site/img/demo-poster.webp python3 scripts/demo/encode.py docs

FFMPEG may be any ffmpeg with libvpx (Playwright ships a small one under its browsers cache). Everything else is Pillow,
so no fps/concat/tile filters or png decoder are needed (frames are piped as MJPEG). Metadata is stripped (no encoder tags, no paths, no usernames).
Optional env: FPS (default 25), WEBP_W (default 800), WEBP_FPS (default 8), WEBP_Q (default 50), CRF (default 28),
POSTER (also write the first frame as a 1280x720 WebP there: the website's video poster), MAX_SECONDS (default 60: refuse longer).
"""
import glob, json, os, shutil, subprocess, sys
from PIL import Image

D = os.environ["DEMO_DIR"]
FF = os.environ.get("FFMPEG", "ffmpeg")
OUT = sys.argv[1] if len(sys.argv) > 1 else "docs"
FPS = int(os.environ.get("FPS", 25))
RAW = D + "/raw"
meta = json.load(open(RAW + "/meta.json"))
W, H = 1280, 720
BG = (20, 18, 15)  # --bg in the dark theme, behind the phone-width clip


def extract(webm, dest):
    shutil.rmtree(dest, ignore_errors=True); os.makedirs(dest)
    subprocess.run([FF, "-v", "error", "-i", webm, "-r", str(FPS), "-start_number", "0", dest + "/%05d.png"], check=True)
    return sorted(glob.glob(dest + "/*.png"))


main = extract(sorted(glob.glob(RAW + "/main/*.webm"))[-1], D + "/fr-main")
phone = extract(sorted(glob.glob(RAW + "/phone/*.webm"))[-1], D + "/fr-phone")
main = main[int(meta["mainOffset"] * FPS):int((meta["mainOffset"] + meta["mainDur"]) * FPS)]
phone = phone[int(meta["phoneOffset"] * FPS):]
print("main frames", len(main), "phone frames", len(phone))

def frame_iter():
    for f in main:
        yield Image.open(f).convert("RGB")
    for f in phone:
        im = Image.open(f).convert("RGB")
        canvas = Image.new("RGB", (W, H), BG)
        canvas.paste(im, ((W - im.width) // 2, 0))
        yield canvas


total = len(main) + len(phone)
print("total frames", total, "seconds", total / FPS)
if total / FPS > float(os.environ.get("MAX_SECONDS", 60)):
    sys.exit(f"demo is {total / FPS:.1f}s, over MAX_SECONDS: shorten the beats in record.mjs")

webm = OUT + "/demo.webm"
ww = int(os.environ.get("WEBP_W", 800)); wh = ww * 9 // 16
step = max(1, round(FPS / int(os.environ.get("WEBP_FPS", 8))))
proc = subprocess.Popen([FF, "-v", "error", "-y", "-f", "image2pipe", "-framerate", str(FPS), "-c:v", "mjpeg", "-i", "/dev/stdin", "-c:v", "libvpx", "-b:v", "0",
                "-crf", os.environ.get("CRF", "28"), "-qmin", "4", "-qmax", "40", "-pix_fmt", "yuv420p", "-an", "-map_metadata", "-1",
                "-fflags", "+bitexact", "-flags:v", "+bitexact", "-auto-alt-ref", "0", webm], stdin=subprocess.PIPE)
import io
frames = []
for i, im in enumerate(frame_iter()):          # stream: never hold 1500 full frames in memory
    buf = io.BytesIO(); im.save(buf, "JPEG", quality=95, subsampling=0); proc.stdin.write(buf.getvalue())
    if i == 0 and os.environ.get("POSTER"):
        im.save(os.environ["POSTER"], "WEBP", quality=80, method=6)
        print(os.environ["POSTER"], os.path.getsize(os.environ["POSTER"]) // 1024, "KB")
    if i % step == 0:
        frames.append(im.resize((ww, wh), Image.LANCZOS))
proc.stdin.close(); proc.wait()
print(webm, os.path.getsize(webm) // 1024, "KB")

dur = int(1000 * step / FPS)
webp = OUT + "/demo.webp"
frames[0].save(webp, save_all=True, append_images=frames[1:], duration=dur, loop=0, quality=int(os.environ.get("WEBP_Q", 50)), method=6)
print(webp, os.path.getsize(webp) // 1024, "KB", len(frames), "frames")
json.dump({"frames": total, "seconds": total / FPS}, open(D + "/encode.json", "w"))
