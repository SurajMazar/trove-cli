"""Records real Trove sessions in a pseudo-terminal, renders them as frames,
voices each scene with Kokoro TTS and assembles an MP4 with ffmpeg.

Run with the venv python: venv/bin/python make_video.py OUTPUT.mp4
"""
import copy
import fcntl
import os
import pty
import re
import select
import signal
import struct
import subprocess
import sys
import termios
import time

import numpy as np
import pyte
import soundfile as sf
from PIL import Image, ImageDraw, ImageFont, ImageFilter

V = os.path.dirname(os.path.abspath(__file__))
DEMO = os.path.join(V, "demo")
TROVE = os.path.join(V, "trove")
OUT = sys.argv[1] if len(sys.argv) > 1 else os.path.join(V, "trove-demo.mp4")
FRAMES = os.path.join(V, "frames")
COLS, ROWS = 100, 28
W, H = 1920, 1080
FPS = 30
SR = 24000

# --------------------------------------------------------------------------
# Scenes
# --------------------------------------------------------------------------

SCENES = [
    {"kind": "card", "title": "Trove", "subtitle": "One CLI for every Git forge",
     "lines": ["GitHub  ·  GitLab  ·  Bitbucket  ·  your own forge"],
     "say": "Meet Trove. One command line for GitHub, GitLab, Bitbucket, and your own Git servers."},
    {"kind": "term", "caption": "Every account in one place", "cwd": "code",
     "cmds": [("trove provider list", []),
              ("trove provider use", [(1.3, "/"), (0.7, "hub"), (1.1, "\r"), (0.8, "\r")])],
     "say": "Add all of your accounts, personal and work. Credentials stay in your keychain or Bitwarden, "
            "never in the config file, and switching accounts is a quick search away."},
    {"kind": "term", "caption": "A dashboard for your forge", "cwd": "code",
     "cmds": [("trove", [(3.2, "\x1b[B"), (0.7, "\x1b[B"), (0.7, "\x1b[B"), (1.6, "q")])],
     "say": "Run trove on its own and you get a dashboard: who you're signed in as, your repositories, "
            "open pull requests and issues, and what you can do next."},
    {"kind": "term", "caption": "Pick repositories, clone in parallel", "cwd": "code/projects",
     "cmds": [("trove repo clone", [(1.8, "/"), (0.7, "api"), (1.2, "\r"), (0.7, " "), (0.6, " "), (0.9, "\x1b"),
                                    (0.9, "\x1b[B"), (0.6, " "), (1.1, "\r")])],
     "say": "Cloning is interactive. Search as you type, pick as many repositories as you like, "
            "and Trove clones them in parallel, with a clear summary when it's done."},
    {"kind": "term", "caption": "Find and read issues", "cwd": "code",
     "cmds": [("trove issue browse -R acme/web-app",
               [(1.8, "/"), (0.6, "author:maya"), (1.3, "\r"), (1.0, "\r"), (2.6, "j"), (0.35, "j"), (0.35, "j"),
                (0.35, "j"), (1.6, "\x1b"), (1.4, "q")])],
     "say": "Browse the issues of any repository. Search by title, or filter by author, "
            "then open one to read its full description, labels and links, right in your terminal. "
            "Press escape to go back to the list."},
    {"kind": "term", "caption": "Commit, push, open a pull request", "cwd": "code/web-app",
     "cmds": [('trove commit -a -m "Add dark mode toggle" --push --pr', [(2.8, "\r"), (1.2, "\r")])],
     "say": "When you're done, commit and push with the right credentials for each account, "
            "and open a pull request in the same step."},
    {"kind": "card", "title": "Trove", "subtitle": "Open source · MIT",
     "lines": ["go install github.com/SurajMazar/trove-cli/cmd/trove@latest", "github.com/SurajMazar/trove-cli"],
     "say": "Trove is open source. Install it with go install, or find it on GitHub at Suraj Mazar, slash trove C-L-I."},
]

# --------------------------------------------------------------------------
# Recording
# --------------------------------------------------------------------------


def env_for():
    e = dict(os.environ)
    e.pop("CI", None)
    e.update({
        "TERM": "xterm-256color", "COLORTERM": "truecolor", "COLUMNS": str(COLS), "LINES": str(ROWS),
        "TROVE_CONFIG": os.path.join(DEMO, "config.yaml"), "TROVE_TOKEN_GITHUB_PERSONAL": "demo-token",
        "XDG_CACHE_HOME": os.path.join(DEMO, "cache"), "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
        "GIT_AUTHOR_NAME": "Alex Rivera", "GIT_AUTHOR_EMAIL": "alex@acme.dev",
        "GIT_COMMITTER_NAME": "Alex Rivera", "GIT_COMMITTER_EMAIL": "alex@acme.dev",
        "PATH": os.path.dirname(TROVE) + ":" + os.environ["PATH"],
    })
    return e


def record(cmd, keys, cwd):
    """Runs cmd in a pty; returns [(t, bytes)] relative to start."""
    argv = ["/bin/sh", "-c", cmd.replace("trove", TROVE, 1)]
    pid, fd = pty.fork()
    if pid == 0:
        fcntl.ioctl(0, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))
        os.chdir(cwd)
        os.execve("/bin/sh", argv, env_for())
    start = time.time()
    events, pending = [], list(keys)
    next_at = start + (pending[0][0] if pending else 0)
    done = False
    while True:
        now = time.time()
        if pending and now >= next_at:
            os.write(fd, pending.pop(0)[1].encode())
            next_at = now + (pending[0][0] if pending else 0)
        r, _, _ = select.select([fd], [], [], 0.02)
        if r:
            try:
                data = os.read(fd, 65536)
            except OSError:
                data = b""
            if not data:
                break
            if b"\x1b]11;?" in data:
                os.write(fd, b"\x1b]11;rgb:0f0f/1111/1a1a\x1b\\")
            if b"\x1b]10;?" in data:
                os.write(fd, b"\x1b]10;rgb:c0c0/caca/f5f5\x1b\\")
            if b"\x1b[6n" in data:
                os.write(fd, b"\x1b[1;1R")
            events.append((time.time() - start, data))
        p, _ = os.waitpid(pid, os.WNOHANG)
        if p and not pending:
            done = True
        if done and not r:
            break
        if time.time() - start > 60:
            os.kill(pid, signal.SIGKILL)
            raise RuntimeError(f"timeout recording {cmd!r}")
    try:
        os.waitpid(pid, 0)
    except ChildProcessError:
        pass
    return events, time.time() - start


PROMPT = "\x1b[38;5;111m~/{cwd}\x1b[0m \x1b[38;5;141m❯\x1b[0m "


def scene_stream(sc):
    """Typed prompt + recorded program output for each command, as one stream."""
    stream, t = [], 0.35
    cwd = os.path.join(DEMO, sc["cwd"])
    os.makedirs(cwd, exist_ok=True)
    for cmd, keys in sc["cmds"]:
        stream.append((t, PROMPT.format(cwd=sc["cwd"]).encode()))
        t += 0.45
        for ch in cmd:
            stream.append((t, ch.encode()))
            t += 0.045
        t += 0.35
        stream.append((t, b"\r\n"))
        events, dur = record(cmd, keys, cwd)
        stream += [(t + et, data) for et, data in events]
        t += dur + 0.6
    stream.append((t, PROMPT.format(cwd=sc["cwd"]).encode()))
    return stream, t + 0.8

# --------------------------------------------------------------------------
# Rendering
# --------------------------------------------------------------------------

FONT_SIZE = 21
MONO = ImageFont.truetype("/System/Library/Fonts/Menlo.ttc", FONT_SIZE, index=0)
MONO_B = ImageFont.truetype("/System/Library/Fonts/Menlo.ttc", FONT_SIZE, index=1)
FALLBACKS = [ImageFont.truetype(p, FONT_SIZE) for p in (
    "/System/Library/Fonts/Apple Symbols.ttf", "/System/Library/Fonts/Apple Braille.ttf", "/System/Library/Fonts/SFNSMono.ttf")]
SANS = "/System/Library/Fonts/SFNS.ttf"
CW = MONO.getlength("M")
LH = int(FONT_SIZE * 1.32)
TERM_W, TERM_H = int(CW * COLS), LH * ROWS

PALETTE = {
    "default": "#c0caf5", "black": "#414868", "red": "#f7768e", "green": "#9ece6a", "brown": "#e0af68",
    "yellow": "#e0af68", "blue": "#7aa2f7", "magenta": "#bb9af7", "cyan": "#7dcfff", "white": "#c0caf5",
    "brightblack": "#6b7089", "brightred": "#ff899d", "brightgreen": "#9fe044", "brightyellow": "#faba4a",
    "brightbrown": "#faba4a", "brightblue": "#8db0ff", "brightmagenta": "#c7a9ff", "brightcyan": "#a4daff",
    "brightwhite": "#ffffff",
}
BG = "#11131c"

def _raster(f, ch):
    im = Image.new("L", (48, 48))
    ImageDraw.Draw(im).text((4, 4), ch, font=f, fill=255)
    return im.tobytes()


_glyph_font = {}


def font_for(ch, bold):
    """Menlo first; symbols it lacks (braille spinner, ◆, ✓) fall back."""
    key = (ch, bold)
    if key not in _glyph_font:
        f = MONO_B if bold else MONO
        if ord(ch) > 127 and _raster(f, ch) == _raster(f, "\U000F0000"):
            f = next((fb for fb in FALLBACKS if _raster(fb, ch) != _raster(fb, "\U000F0000")), f)
        _glyph_font[key] = f
    return _glyph_font[key]


def color(c, default):
    if c in ("default", None, ""):
        return default
    if c in PALETTE:
        return PALETTE[c]
    if re.fullmatch(r"[0-9a-fA-F]{6}", c):
        return "#" + c
    return default


# Box-drawing characters are drawn as lines spanning the whole cell (as real
# terminals do) so borders join across the taller line height.
BOX = {"─": "lr", "━": "lr", "│": "ud", "┃": "ud", "┌": "rd", "┐": "ld", "└": "ru", "┘": "lu",
       "├": "udr", "┤": "udl", "┬": "lrd", "┴": "lru", "┼": "lrud", "╭": "Rd", "╮": "Ld", "╰": "Ru", "╯": "Lu"}


def draw_box(d, ch, px, py, fg):
    spec = BOX[ch]
    cx, cy = px + CW / 2, py + LH / 2
    w = 2
    if spec[0] in "RL":  # rounded corner: arc + straight tail to the edges
        r = CW / 2
        horiz_right = spec[0] == "R"
        down = spec[1] == "d"
        ax = cx + (r if horiz_right else -r)
        ay = cy + (r if down else -r)
        start = {(True, True): 180, (False, True): 270, (True, False): 90, (False, False): 0}[(horiz_right, down)]
        d.arc([ax - r, ay - r, ax + r, ay + r], start, start + 90, fill=fg, width=w)
        if horiz_right:
            d.line([ax, cy, px + CW + 0.5, cy], fill=fg, width=w)
        else:
            d.line([px, cy, ax, cy], fill=fg, width=w)
        if down:
            d.line([cx, ay, cx, py + LH + 0.5], fill=fg, width=w)
        else:
            d.line([cx, py, cx, ay], fill=fg, width=w)
        return
    if "l" in spec:
        d.line([px, cy, cx, cy], fill=fg, width=w)
    if "r" in spec:
        d.line([cx, cy, px + CW + 0.5, cy], fill=fg, width=w)
    if "u" in spec:
        d.line([cx, py, cx, cy], fill=fg, width=w)
    if "d" in spec:
        d.line([cx, cy, cx, py + LH + 0.5], fill=fg, width=w)


def render_term(screen):
    img = Image.new("RGB", (TERM_W, TERM_H), BG)
    d = ImageDraw.Draw(img)
    for y in range(ROWS):
        line = screen.buffer[y]
        x = 0
        while x < COLS:
            ch = line[x]
            fg, bg = color(ch.fg, PALETTE["default"]), color(ch.bg, BG)
            if ch.reverse:
                fg, bg = bg, fg
            px, py = x * CW, y * LH
            if bg != BG:
                d.rectangle([px, py, px + CW + 0.5, py + LH], fill=bg)
            if ch.data in BOX:
                draw_box(d, ch.data, px, py, fg)
            elif ch.data.strip():
                f = font_for(ch.data, ch.bold)
                d.text((px, py + (LH - FONT_SIZE) / 2 - 1), ch.data, font=f, fill=fg)
            x += 1
    if not screen.cursor.hidden:
        cx, cy = screen.cursor.x * CW, screen.cursor.y * LH
        d.rectangle([cx, cy + 3, cx + CW - 1, cy + LH - 3], fill="#c0caf5")
    return img


def gradient_bg():
    top, bottom = np.array([28, 20, 56]), np.array([12, 28, 48])
    t = np.linspace(0, 1, H)[:, None, None]
    arr = (top * (1 - t) + bottom * t).astype(np.uint8)
    arr = np.repeat(arr, W, axis=1)
    return Image.fromarray(arr, "RGB")


BACKGROUND = gradient_bg()
WIN_PAD, TITLE_H = 26, 44
WIN_W, WIN_H = TERM_W + 2 * WIN_PAD, TERM_H + TITLE_H + 2 * WIN_PAD - 8
WIN_X, WIN_Y = (W - WIN_W) // 2, 128


def chrome(caption):
    base = BACKGROUND.copy()
    shadow = Image.new("RGBA", (W, H), (0, 0, 0, 0))
    ImageDraw.Draw(shadow).rounded_rectangle([WIN_X + 6, WIN_Y + 16, WIN_X + WIN_W + 6, WIN_Y + WIN_H + 16], 18, fill=(0, 0, 0, 150))
    base.paste(shadow.filter(ImageFilter.GaussianBlur(18)), (0, 0), shadow.filter(ImageFilter.GaussianBlur(18)))
    d = ImageDraw.Draw(base)
    d.rounded_rectangle([WIN_X, WIN_Y, WIN_X + WIN_W, WIN_Y + WIN_H], 16, fill=BG, outline="#2a2f45", width=2)
    d.rounded_rectangle([WIN_X, WIN_Y, WIN_X + WIN_W, WIN_Y + TITLE_H], 16, fill="#1b1e2b")
    d.rectangle([WIN_X, WIN_Y + TITLE_H - 16, WIN_X + WIN_W, WIN_Y + TITLE_H], fill="#1b1e2b")
    for i, c in enumerate(("#ff5f57", "#febc2e", "#28c840")):
        cx = WIN_X + 24 + i * 24
        d.ellipse([cx - 7, WIN_Y + TITLE_H / 2 - 7, cx + 7, WIN_Y + TITLE_H / 2 + 7], fill=c)
    tf = ImageFont.truetype(SANS, 17)
    title = "trove — zsh — 100×28"
    d.text((WIN_X + WIN_W / 2 - tf.getlength(title) / 2, WIN_Y + 12), title, font=tf, fill="#8b90a8")
    cf = ImageFont.truetype(SANS, 44)
    cf.set_variation_by_name("Semibold") if hasattr(cf, "set_variation_by_name") else None
    d.text((W / 2 - cf.getlength(caption) / 2, 48), caption, font=cf, fill="#ffffff")
    return base


def card(sc):
    img = BACKGROUND.copy()
    d = ImageDraw.Draw(img)
    big = ImageFont.truetype(SANS, 150)
    try:
        big.set_variation_by_name("Bold")
    except Exception:
        pass
    mid = ImageFont.truetype(SANS, 52)
    small = ImageFont.truetype("/System/Library/Fonts/Menlo.ttc", 30)
    diamond = "◆ "
    dfont = FALLBACKS[0].font_variant(size=120)
    tw = dfont.getlength(diamond) + big.getlength(sc["title"])
    x = W / 2 - tw / 2
    d.text((x, 300), diamond, font=dfont, fill="#bb9af7")
    d.text((x + dfont.getlength(diamond), 280), sc["title"], font=big, fill="#ffffff")
    d.text((W / 2 - mid.getlength(sc["subtitle"]) / 2, 480), sc["subtitle"], font=mid, fill="#c0caf5")
    y = 610
    for line in sc["lines"]:
        f = small if ("/" in line or "go " in line) else mid
        d.text((W / 2 - f.getlength(line) / 2, y), line, font=f, fill="#7dcfff" if f is small else "#9aa5ce")
        y += 70
    return img


def split_altscreen(data):
    return re.split(rb"(\x1b\[\?1049[hl])", data)


def render_scene(sc, idx, stream, dur, frames):
    screen = pyte.Screen(COLS, ROWS)
    bs = pyte.ByteStream(screen)
    saved = None
    base = chrome(sc["caption"])
    last_key, t_prev, first = None, 0.0, True
    out = []

    def snap(t):
        nonlocal last_key, t_prev, first
        key = tuple("".join(screen.buffer[y][x].data for x in range(COLS)) for y in range(ROWS)) + (
            screen.cursor.x, screen.cursor.y, screen.cursor.hidden,
            tuple((c.fg, c.bold, c.reverse, c.bg) for y in range(ROWS) for c in screen.buffer[y].values()))
        if key == last_key:
            return
        img = base.copy()
        img.paste(render_term(screen), (WIN_X + WIN_PAD, WIN_Y + TITLE_H + WIN_PAD - 8))
        path = os.path.join(frames, f"s{idx:02d}_{len(out):05d}.png")
        img.save(path, optimize=False, compress_level=1)
        out.append([path, t])
        last_key = key

    snap(0.0)
    # Group events by frame time so bursty output renders once per frame.
    i = 0
    while i < len(stream):
        t = stream[i][0]
        frame_t = round(t * FPS) / FPS
        while i < len(stream) and round(stream[i][0] * FPS) / FPS == frame_t:
            for part in split_altscreen(stream[i][1]):
                if part == b"\x1b[?1049h":
                    saved = (copy.deepcopy(screen.buffer), copy.copy(screen.cursor))
                    screen.reset()
                elif part == b"\x1b[?1049l" and saved:
                    screen.buffer.clear()
                    screen.buffer.update(saved[0])
                    screen.cursor = saved[1]
                    saved = None
                elif part:
                    bs.feed(part)
            i += 1
        snap(frame_t)
    # durations
    timeline = []
    for j, (path, t) in enumerate(out):
        nxt = out[j + 1][1] if j + 1 < len(out) else dur
        timeline.append((path, max(nxt - t, 1.0 / FPS)))
    return timeline

# --------------------------------------------------------------------------
# Narration
# --------------------------------------------------------------------------


def make_voice(texts):
    import espeakng_loader
    here = os.getcwd()
    os.chdir(os.path.dirname(espeakng_loader.get_data_path()))
    os.environ["ESPEAK_DATA_PATH"] = "espeak-ng-data"  # espeak-ng rejects long absolute paths
    from kokoro_onnx import Kokoro
    k = Kokoro(os.path.join(V, "models/kokoro-v1.0.fp16.onnx"), os.path.join(V, "models/voices-v1.0.bin"))
    clips = []
    for t in texts:
        samples, sr = k.create(t, voice="af_heart", speed=1.0, lang="en-us")
        assert sr == SR
        clips.append(samples.astype(np.float32))
    os.chdir(here)
    return clips

# --------------------------------------------------------------------------
# Main
# --------------------------------------------------------------------------


def main():
    subprocess.run(["sh", os.path.join(V, "setup_demo.sh")], check=True, stdout=subprocess.DEVNULL)
    server = subprocess.Popen([sys.executable, os.path.join(V, "fake_github.py"), "18800", os.path.join(DEMO, "src")])
    time.sleep(0.8)
    try:
        print("voicing...", flush=True)
        clips = make_voice([s["say"] for s in SCENES])
        os.makedirs(FRAMES, exist_ok=True)
        for f in os.listdir(FRAMES):
            os.remove(os.path.join(FRAMES, f))
        concat, audio, t0 = [], [], 0.0
        LEAD = 0.45
        for idx, sc in enumerate(SCENES):
            voice = clips[idx]
            vlen = len(voice) / SR
            if sc["kind"] == "card":
                dur = vlen + LEAD + 1.0
                path = os.path.join(FRAMES, f"s{idx:02d}_card.png")
                card(sc).save(path)
                timeline = [(path, dur)]
            else:
                print(f"recording scene {idx}: {sc['caption']}", flush=True)
                stream, vis = scene_stream(sc)
                dur = max(vis, vlen + LEAD + 0.9)
                timeline = render_scene(sc, idx, stream, dur, FRAMES)
            concat += timeline
            audio.append((t0 + LEAD, voice))
            t0 += dur
            print(f"  scene {idx}: {dur:.1f}s (voice {vlen:.1f}s, {len(timeline)} frames)", flush=True)
        total = t0
        track = np.zeros(int(total * SR) + SR, dtype=np.float32)
        for start, clip in audio:
            s = int(start * SR)
            track[s:s + len(clip)] += clip
        peak = float(np.max(np.abs(track))) or 1.0
        track = track / peak * 0.89
        wav = os.path.join(V, "narration.wav")
        sf.write(wav, track, SR)
        lst = os.path.join(V, "frames.txt")
        with open(lst, "w") as f:
            for path, d in concat:
                f.write(f"file '{path}'\nduration {d:.4f}\n")
            f.write(f"file '{concat[-1][0]}'\n")
        subprocess.run(["ffmpeg", "-y", "-hide_banner", "-loglevel", "error", "-f", "concat", "-safe", "0", "-i", lst,
                        "-i", wav, "-vf", f"fps={FPS},format=yuv420p,fade=t=in:st=0:d=0.6,fade=t=out:st={total - 0.8:.2f}:d=0.8",
                        "-af", f"afade=t=out:st={total - 0.8:.2f}:d=0.8",
                        "-c:v", "libx264", "-preset", "slow", "-crf", "24", "-tune", "animation",
                        "-c:a", "aac", "-b:a", "128k", "-movflags", "+faststart", "-t", f"{total:.2f}", OUT], check=True)
        print(f"wrote {OUT} ({total:.1f}s)")
    finally:
        server.terminate()


if __name__ == "__main__":
    main()
